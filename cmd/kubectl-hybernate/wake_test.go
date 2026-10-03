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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

var wakeTime = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

var apiKey = client.ObjectKey{Namespace: "sandbox-42", Name: "api"}

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
		{name: "a paused workload", phase: v1alpha1.PhasePaused, wantOutput: "waking sandbox-42/api\n"},
		{
			name: "kept awake for a while", phase: v1alpha1.PhasePaused, keepAwake: 2 * time.Hour,
			wantActiveUntil: "2026-10-03T11:00:00Z", wantOutput: "waking sandbox-42/api\n",
		},
		{
			name: "a running workload", phase: v1alpha1.PhaseRunning,
			wantOutput: "sandbox-42/api is already running; its idle clock restarts now\n",
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

	assert.Contains(t, out.String(), "sandbox-42/api is Running after")
	assert.GreaterOrEqual(t, gets, 4, "it polls until the workload is Running")
}

func TestWake_TimesOut(t *testing.T) {
	c := newClient(t, interceptor.Funcs{}, managedWorkload(v1alpha1.PhasePaused))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := wake(ctx, c, apiKey, testOptions(), &bytes.Buffer{})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "still Paused")
	assert.Contains(t, err.Error(), "kubectl describe managedworkload api -n sandbox-42")
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
		{name: "destroyed", workload: managedWorkload(v1alpha1.PhaseDestroyed), wantHint: "redeploy it"},
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
