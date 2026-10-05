/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

var wakeTime = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

var apiKey = client.ObjectKey{Namespace: "preview-42", Name: "api"}

func managedWorkload(phase v1alpha1.WorkloadPhase) *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: apiKey.Name, Namespace: apiKey.Namespace},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: phase},
	}
}

func testOptions() wakeOptions {
	return wakeOptions{wait: true, pollInterval: time.Millisecond, now: func() time.Time { return wakeTime }}
}

// testContext bounds each wake, so a bug that keeps it waiting fails the
// test instead of hanging until go test's timeout.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newClient(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

func annotations(t *testing.T, c client.Client) map[string]string {
	t.Helper()
	var w v1alpha1.ManagedWorkload
	require.NoError(t, c.Get(context.Background(), apiKey, &w))
	return w.Annotations
}

func TestWake_StampsActivity(t *testing.T) {
	tests := []struct {
		name            string
		phase           v1alpha1.WorkloadPhase
		keepAwake       time.Duration
		wantActiveUntil string
		wantOutput      string
	}{
		{name: "a paused workload", phase: v1alpha1.PhasePaused, wantOutput: "waking preview-42/api\n"},
		{
			name: "kept awake for a while", phase: v1alpha1.PhasePaused, keepAwake: 2 * time.Hour,
			wantActiveUntil: "2026-10-03T11:00:00Z", wantOutput: "waking preview-42/api\n",
		},
		{
			name: "a running workload", phase: v1alpha1.PhaseRunning,
			wantOutput: "preview-42/api is already running; its idle clock restarts now\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, interceptor.Funcs{}, managedWorkload(tt.phase))
			opts := testOptions()
			opts.wait = false
			opts.keepAwake = tt.keepAwake
			var out bytes.Buffer

			require.NoError(t, wake(testContext(t), c, apiKey, opts, &out))

			got := annotations(t, c)
			assert.Equal(t, "2026-10-03T09:00:00Z", got[v1alpha1.AnnotationLastActivity])
			assert.Equal(t, tt.wantActiveUntil, got[v1alpha1.AnnotationActiveUntil])
			assert.Equal(t, tt.wantOutput, out.String())
		})
	}
}

func TestWake_WaitsUntilRunning(t *testing.T) {
	var gets int
	becomesRunning := func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		gets++
		if w, ok := obj.(*v1alpha1.ManagedWorkload); ok && gets >= 4 {
			w.Status.Phase = v1alpha1.PhaseRunning
		}
		return nil
	}
	funcs := interceptor.Funcs{Get: becomesRunning}
	c := newClient(t, funcs, managedWorkload(v1alpha1.PhasePaused))
	var out bytes.Buffer

	require.NoError(t, wake(testContext(t), c, apiKey, testOptions(), &out))

	assert.Contains(t, out.String(), "preview-42/api is Running after")
	assert.GreaterOrEqual(t, gets, 4, "it polls until the workload is Running")
}

func TestWake_TimesOut(t *testing.T) {
	c := newClient(t, interceptor.Funcs{}, managedWorkload(v1alpha1.PhasePaused))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := wake(ctx, c, apiKey, testOptions(), &bytes.Buffer{})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "still Paused")
	assert.Contains(t, err.Error(), "kubectl describe managedworkload api -n preview-42")
}

func TestWake_RefusesWhatActivityCantWake(t *testing.T) {
	manual := managedWorkload(v1alpha1.PhasePaused)
	manual.Spec.DesiredState = ptr.To(v1alpha1.DesiredStatePaused)

	tests := []struct {
		name     string
		workload *v1alpha1.ManagedWorkload
		wantHint string
	}{
		{name: "paused by desiredState", workload: manual, wantHint: "remove it or set it to Running"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, interceptor.Funcs{}, tt.workload)

			err := wake(testContext(t), c, apiKey, testOptions(), &bytes.Buffer{})

			require.ErrorIs(t, err, errNotWakeable)
			assert.Contains(t, err.Error(), tt.wantHint)
			assert.Empty(t, annotations(t, c), "nothing is stamped when it couldn't wake it")
		})
	}
}

func TestWake_NotFound(t *testing.T) {
	c := newClient(t, interceptor.Funcs{})

	err := wake(testContext(t), c, apiKey, testOptions(), &bytes.Buffer{})

	assert.True(t, apierrors.IsNotFound(err))
}

// Wake takes the workload's name as status shows it, as well as its
// ManagedWorkload's.
func TestWake_ByWorkloadName(t *testing.T) {
	handWritten := managedWorkload(v1alpha1.PhaseRunning)
	handWritten.Name = "api-mw"
	postgres := managedWorkload(v1alpha1.PhaseRunning)
	postgres.Name = "db"
	postgres.Spec.Target = v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres"}

	tests := []struct {
		name, arg, want string
	}{
		{name: "by the workload", arg: "api", want: "api-mw"},
		{name: "by kind and workload", arg: "statefulset/postgres", want: "db"},
		{name: "by the ManagedWorkload", arg: "db", want: "db"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, interceptor.Funcs{}, handWritten.DeepCopy(), postgres.DeepCopy())
			opts := testOptions()
			opts.wait = false

			require.NoError(t, wake(testContext(t), c, client.ObjectKey{Namespace: "preview-42", Name: tt.arg}, opts,
				&bytes.Buffer{}))

			var w v1alpha1.ManagedWorkload
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "preview-42", Name: tt.want}, &w))
			assert.Equal(t, "2026-10-03T09:00:00Z", w.Annotations[v1alpha1.AnnotationLastActivity])
		})
	}
}

func TestWake_AmbiguousName(t *testing.T) {
	deployment := managedWorkload(v1alpha1.PhasePaused)
	deployment.Name = "api-deploy"
	statefulSet := managedWorkload(v1alpha1.PhasePaused)
	statefulSet.Name = "api-sts"
	statefulSet.Spec.Target.Kind = v1alpha1.TargetKindStatefulSet
	c := newClient(t, interceptor.Funcs{}, deployment, statefulSet)

	err := wake(testContext(t), c, client.ObjectKey{Namespace: "preview-42", Name: "api"}, testOptions(), &bytes.Buffer{})

	require.ErrorIs(t, err, errAmbiguous)
	assert.EqualError(t, err, "api in preview-42 matches more than one ManagedWorkload: "+
		"api-deploy (deployment/api), api-sts (statefulset/api); name one by its workload's kind and name, "+
		"such as deployment/api")
}

// A ManagedWorkload named api that manages another workload doesn't win
// over one that manages a workload named api: the bare name means either,
// and the workload picks one.
func TestWake_NameOfOneTargetOfAnother(t *testing.T) {
	backend := managedWorkload(v1alpha1.PhasePaused)
	backend.Spec.Target.Name = "backend"
	frontend := managedWorkload(v1alpha1.PhasePaused)
	frontend.Name = "frontend-mw"
	opts := testOptions()
	opts.wait = false

	err := wake(testContext(t), newClient(t, interceptor.Funcs{}, backend.DeepCopy(), frontend.DeepCopy()), apiKey,
		opts, &bytes.Buffer{})

	require.ErrorIs(t, err, errAmbiguous)
	assert.ErrorContains(t, err, "api (deployment/backend), frontend-mw (deployment/api)")

	for arg, want := range map[string]string{"deployment/backend": "api", "deployment/api": "frontend-mw"} {
		t.Run(arg, func(t *testing.T) {
			c := newClient(t, interceptor.Funcs{}, backend.DeepCopy(), frontend.DeepCopy())

			require.NoError(t, wake(testContext(t), c, client.ObjectKey{Namespace: "preview-42", Name: arg}, opts,
				&bytes.Buffer{}))

			var w v1alpha1.ManagedWorkload
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "preview-42", Name: want}, &w))
			assert.NotEmpty(t, w.Annotations[v1alpha1.AnnotationLastActivity])
		})
	}
}

// When a Deployment and a StatefulSet share a name, the operator names one
// ManagedWorkload after the workload and the other after its kind too. The
// bare name then means either, and the kind picks one.
func TestWake_SharedWorkloadName(t *testing.T) {
	deployment := managedWorkload(v1alpha1.PhasePaused)
	statefulSet := managedWorkload(v1alpha1.PhasePaused)
	statefulSet.Name = "api-statefulset"
	statefulSet.Spec.Target.Kind = v1alpha1.TargetKindStatefulSet
	opts := testOptions()
	opts.wait = false

	err := wake(testContext(t), newClient(t, interceptor.Funcs{}, deployment.DeepCopy(), statefulSet.DeepCopy()),
		apiKey, opts, &bytes.Buffer{})

	require.ErrorIs(t, err, errAmbiguous)

	for arg, want := range map[string]string{"statefulset/api": "api-statefulset", "deployment/api": "api"} {
		t.Run(arg, func(t *testing.T) {
			c := newClient(t, interceptor.Funcs{}, deployment.DeepCopy(), statefulSet.DeepCopy())

			require.NoError(t, wake(testContext(t), c, client.ObjectKey{Namespace: "preview-42", Name: arg}, opts,
				&bytes.Buffer{}))

			var w v1alpha1.ManagedWorkload
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "preview-42", Name: want}, &w))
			assert.NotEmpty(t, w.Annotations[v1alpha1.AnnotationLastActivity])
		})
	}
}

// Whatever phase the workload is in, wake says what happens and waits
// until it's Running.
func TestWake_FromEachPhase(t *testing.T) {
	tests := []struct {
		phase   v1alpha1.WorkloadPhase
		dryRun  bool
		wantOut string
		waits   bool
	}{
		{phase: v1alpha1.PhasePausing, wantOut: "preview-42/api is pausing; it wakes once the pause finishes...\n",
			waits: true},
		{phase: v1alpha1.PhaseResuming, wantOut: "preview-42/api is already waking...\n", waits: true},
		{phase: v1alpha1.PhaseIdle, wantOut: "preview-42/api was idle; marked active, it stays up...\n", waits: true},
		{phase: v1alpha1.PhaseIdle, dryRun: true,
			wantOut: "preview-42/api is in dry-run, so Hybernate never paused it; its idle clock restarts now\n"},
		{phase: v1alpha1.PhasePaused, dryRun: true, wantOut: "waking preview-42/api...\n", waits: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s dry-run=%t", tt.phase, tt.dryRun), func(t *testing.T) {
			var gets int
			funcs := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				gets++
				if w, ok := obj.(*v1alpha1.ManagedWorkload); ok && gets >= 3 {
					w.Status.Phase = v1alpha1.PhaseRunning
				}
				return nil
			}}
			w := managedWorkload(tt.phase)
			w.Spec.DryRun = tt.dryRun
			c := newClient(t, funcs, w)
			var out bytes.Buffer

			require.NoError(t, wake(testContext(t), c, apiKey, testOptions(), &out))

			want := tt.wantOut
			if tt.waits {
				want += "preview-42/api is Running after 0s\n"
			}
			assert.Equal(t, want, out.String())
			assert.Equal(t, "2026-10-03T09:00:00Z", annotations(t, c)[v1alpha1.AnnotationLastActivity])
		})
	}
}

func TestWake_NotManaged(t *testing.T) {
	ignored := managedWorkload(v1alpha1.PhaseRunning)
	ignored.Status.Conditions = []metav1.Condition{{Type: "TargetAvailable", Status: metav1.ConditionFalse,
		Reason: "TargetIgnored", Message: "Deployment api has hybernate.io/ignore label"}}
	c := newClient(t, interceptor.Funcs{}, ignored)

	err := wake(testContext(t), c, apiKey, testOptions(), &bytes.Buffer{})

	require.ErrorIs(t, err, errNotWakeable)
	assert.Contains(t, err.Error(), "Deployment api has hybernate.io/ignore label")
}

func TestWake_NotInstalled(t *testing.T) {
	funcs := interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object,
		...client.GetOption) error {
		return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "hybernate.io", Kind: "ManagedWorkload"}}
	}}

	err := wake(testContext(t), newClient(t, funcs), apiKey, testOptions(), &bytes.Buffer{})

	assert.ErrorIs(t, err, errNotInstalled)
}

func TestWake_Forbidden(t *testing.T) {
	funcs := interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object,
		...client.GetOption) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: "hybernate.io", Resource: "managedworkloads"}, "api",
			errors.New("user can't get"))
	}}

	err := wake(testContext(t), newClient(t, funcs), apiKey, testOptions(), &bytes.Buffer{})

	require.True(t, apierrors.IsForbidden(err))
	assert.Contains(t, err.Error(),
		`getting ManagedWorkload preview-42/api: managedworkloads.hybernate.io "api" is forbidden`)
}
