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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// operator stands in for Hybernate: once the plugin has read the
// ManagedWorkload to find it, every later read shows answer applied, as
// the operator would have by then.
func operator(answer func(w *v1alpha1.ManagedWorkload)) interceptor.Funcs {
	reads := 0
	return interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
		obj client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		if w, ok := obj.(*v1alpha1.ManagedWorkload); ok {
			reads++
			if reads > 1 {
				answer(w)
			}
		}
		return nil
	}}
}

// pauses answers a request the way the operator does when it pauses the
// workload.
func pauses(w *v1alpha1.ManagedWorkload) {
	w.Status.Phase = v1alpha1.PhasePaused
	answer(w, metav1.ConditionTrue, "Pausing", "pausing as requested")
}

// refuses answers a request the way the operator does when it won't pause
// the workload, leaving it in phase.
func refuses(phase v1alpha1.WorkloadPhase, reason, message string) func(*v1alpha1.ManagedWorkload) {
	return func(w *v1alpha1.ManagedWorkload) {
		w.Status.Phase = phase
		answer(w, metav1.ConditionFalse, reason, message)
	}
}

func answer(w *v1alpha1.ManagedWorkload, status metav1.ConditionStatus, reason, message string) {
	if token := w.Annotations[v1alpha1.AnnotationPauseRequested]; token != "" {
		w.Status.LastPauseRequest = token
	}
	meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{Type: "PauseRequest", Status: status,
		Reason: reason, Message: message})
}

func pauseRequested(t *testing.T, c client.Client) string {
	t.Helper()
	return annotations(t, c)[v1alpha1.AnnotationPauseRequested]
}

// Whatever phase the workload is in, pause says what happens and waits
// until it's Paused. One paused or pausing already isn't asked again.
func TestPause_FromEachPhase(t *testing.T) {
	const paused = "preview-42/api is Paused after 0s; a request to it, or kubectl hybernate wake, wakes it\n"
	tests := []struct {
		phase       v1alpha1.WorkloadPhase
		want        string
		wantRequest bool
	}{
		{phase: v1alpha1.PhaseRunning, want: "pausing preview-42/api...\n" + paused, wantRequest: true},
		{phase: v1alpha1.PhaseIdle, want: "preview-42/api was idle; pausing it now...\n" + paused, wantRequest: true},
		{phase: v1alpha1.PhaseResuming, want: "preview-42/api is waking; it pauses once it's up...\n" + paused,
			wantRequest: true},
		{phase: v1alpha1.PhasePausing, want: "preview-42/api is already pausing...\n" + paused},
		{phase: v1alpha1.PhasePaused,
			want: "preview-42/api is already paused; a request to it, or kubectl hybernate wake, wakes it\n"},
	}
	for _, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			c := newClient(t, operator(pauses), managedWorkload(tt.phase))
			before := time.Now()

			out, err := runCLI(t, c, "pause", "api")

			require.NoError(t, err)
			assert.Equal(t, tt.want, out)
			token := pauseRequested(t, c)
			if !tt.wantRequest {
				assert.Empty(t, token, "nothing is asked of a workload that's pausing already")
				return
			}
			requested, err := time.Parse(time.RFC3339Nano, token)
			require.NoError(t, err, "the token is the time of the request")
			assert.False(t, requested.Before(before.Truncate(time.Second)))
		})
	}
}

// Each request gets a token of its own, so Hybernate acts on a pause asked
// for again after the last one woke.
func TestPause_EachRequestIsNew(t *testing.T) {
	w := managedWorkload(v1alpha1.PhaseRunning)
	w.Annotations = map[string]string{v1alpha1.AnnotationPauseRequested: "2026-10-01T18:00:00Z"}
	w.Status.LastPauseRequest = "2026-10-01T18:00:00Z"
	c := newClient(t, interceptor.Funcs{}, w)

	_, err := runCLI(t, c, "pause", "api", "--wait=false")

	require.NoError(t, err)
	assert.NotEqual(t, "2026-10-01T18:00:00Z", pauseRequested(t, c))
}

func TestPause_NoWait(t *testing.T) {
	c := newClient(t, interceptor.Funcs{}, managedWorkload(v1alpha1.PhaseRunning))

	out, err := runCLI(t, c, "pause", "api", "--wait=false")

	require.NoError(t, err)
	assert.Equal(t, "pausing preview-42/api\n", out)
	assert.NotEmpty(t, pauseRequested(t, c))
}

// Hybernate's answer when it doesn't pause: dry-run and a workload off
// already aren't failures; a reason it won't pause is.
func TestPause_Answers(t *testing.T) {
	tests := []struct {
		name     string
		dryRun   bool
		answer   func(*v1alpha1.ManagedWorkload)
		want     string
		wantErr  string
		wantStep string
	}{
		{
			name: "dry-run", dryRun: true,
			answer: refuses(v1alpha1.PhaseIdle, "DryRun",
				"not paused: dry-run is on, so Hybernate counts a would-be pause and leaves the workload running"),
			wantStep: "preview-42/api is in dry-run, so Hybernate counts a would-be pause and leaves it running...\n",
			want: "preview-42/api wasn't paused: dry-run is on, so Hybernate counts a would-be pause and leaves " +
				"the workload running\n",
		},
		{
			name: "scaled to zero",
			answer: refuses(v1alpha1.PhaseRunning, "ScaledToZero",
				"not paused: it's scaled to zero outside Hybernate, so it's off already"),
			wantStep: "pausing preview-42/api...\n",
			want:     "preview-42/api is scaled to zero outside Hybernate, so it's off already\n",
		},
		{
			name: "protected namespace",
			answer: refuses(v1alpha1.PhaseRunning, "Protected",
				"not paused: namespace preview-42 is protected, so Hybernate doesn't pause workloads in it"),
			wantStep: "pausing preview-42/api...\n",
			wantErr: "preview-42/api wasn't paused: namespace preview-42 is protected, so Hybernate doesn't pause " +
				"workloads in it",
		},
		{
			name:     "awake dependents",
			answer:   refuses(v1alpha1.PhaseRunning, "HeldByDependents", "not paused: kept awake for preview-42/web"),
			wantStep: "pausing preview-42/api...\n",
			wantErr:  "preview-42/api wasn't paused: kept awake for preview-42/web",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := managedWorkload(v1alpha1.PhaseRunning)
			w.Spec.DryRun = tt.dryRun
			c := newClient(t, operator(tt.answer), w)

			out, err := runCLI(t, c, "pause", "api")

			if tt.wantErr != "" {
				require.ErrorIs(t, err, errNotPaused)
				assert.EqualError(t, err, tt.wantErr)
				assert.Equal(t, tt.wantStep, out[:len(tt.wantStep)])
				assert.NotContains(t, out, "Usage:")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantStep+tt.want, out)
		})
	}
}

// An earlier request's answer, still in status, isn't taken for this one's.
func TestPause_WaitsForThisRequestsAnswer(t *testing.T) {
	w := managedWorkload(v1alpha1.PhaseRunning)
	w.Annotations = map[string]string{v1alpha1.AnnotationPauseRequested: "an-earlier-request"}
	w.Status.LastPauseRequest = "an-earlier-request"
	answer(w, metav1.ConditionFalse, "Protected", "not paused: namespace preview-42 is protected")
	c := newClient(t, interceptor.Funcs{}, w)

	_, err := runCLI(t, c, "pause", "api", "--timeout", "300ms")

	require.ErrorIs(t, err, errNotPausedYet)
	assert.NotErrorIs(t, err, errNotPaused)
}

func TestPause_TimesOut(t *testing.T) {
	c := newClient(t, interceptor.Funcs{}, managedWorkload(v1alpha1.PhaseRunning))
	start := time.Now()

	_, err := runCLI(t, c, "pause", "api", "--timeout", "300ms")

	require.ErrorIs(t, err, errNotPausedYet)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "preview-42/api isn't Paused yet: it's still Running")
	assert.Contains(t, err.Error(), "kubectl describe managedworkload api -n preview-42")
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestPause_NotFound(t *testing.T) {
	_, err := runCLI(t, newClient(t, interceptor.Funcs{}), "pause", "api")

	assert.True(t, apierrors.IsNotFound(err))
}

// Pause takes the same names as wake: the workload, kind and workload, or
// the ManagedWorkload, and refuses a bare name that means more than one.
func TestPause_Names(t *testing.T) {
	handWritten := managedWorkload(v1alpha1.PhaseRunning)
	handWritten.Name = "api-mw"
	postgres := managedWorkload(v1alpha1.PhaseRunning)
	postgres.Name = "db"
	postgres.Spec.Target = v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres"}

	for arg, want := range map[string]string{"api": "api-mw", "statefulset/postgres": "db", "db": "db"} {
		t.Run(arg, func(t *testing.T) {
			c := newClient(t, interceptor.Funcs{}, handWritten.DeepCopy(), postgres.DeepCopy())

			_, err := runCLI(t, c, "pause", arg, "--wait=false")

			require.NoError(t, err)
			var w v1alpha1.ManagedWorkload
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "preview-42", Name: want}, &w))
			assert.NotEmpty(t, w.Annotations[v1alpha1.AnnotationPauseRequested])
		})
	}

	t.Run("ambiguous", func(t *testing.T) {
		statefulSet := managedWorkload(v1alpha1.PhaseRunning)
		statefulSet.Name = "api-sts"
		statefulSet.Spec.Target.Kind = v1alpha1.TargetKindStatefulSet
		c := newClient(t, interceptor.Funcs{}, handWritten.DeepCopy(), statefulSet)

		_, err := runCLI(t, c, "pause", "api")

		require.ErrorIs(t, err, errAmbiguous)
		assert.ErrorContains(t, err, "name one by its workload's kind and name, such as deployment/api")
	})
}

// A workload whose replicas Argo CD or Flux set from Git is paused anyway,
// with a warning that the tool will likely undo it, and the one-time fix.
// One a person scaled last gets no warning.
func TestPause_WarnsWhenGitOpsWillUndoIt(t *testing.T) {
	tests := []struct {
		manager, tool, fix string
	}{
		{manager: "argocd-controller", tool: "Argo CD", fix: "resource.customizations.ignoreDifferences.apps_Deployment"},
		{manager: "kustomize-controller", tool: "Flux", fix: "Leave replicas out of the workload's manifest in Git"},
		{manager: "kubectl-scale"},
	}
	for _, tt := range tests {
		t.Run(tt.manager, func(t *testing.T) {
			replicasSetBy := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if d, ok := obj.(*appsv1.Deployment); ok {
					d.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: tt.manager,
						Operation: metav1.ManagedFieldsOperationApply, FieldsType: "FieldsV1",
						FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:replicas":{}}}`)}}}
				}
				return nil
			}}
			api := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "preview-42"}}
			c := newClient(t, replicasSetBy, managedWorkload(v1alpha1.PhaseRunning), api)

			out, err := runCLI(t, c, "pause", "api", "--wait=false")

			require.NoError(t, err)
			assert.NotEmpty(t, pauseRequested(t, c), "it's paused anyway")
			if tt.tool == "" {
				assert.Equal(t, "pausing preview-42/api\n", out)
				return
			}
			assert.Contains(t, out, "Warning: "+tt.tool+" set preview-42/api's replicas last, as "+tt.manager+
				", so it will likely scale it back up on its next sync and undo this pause.")
			assert.Contains(t, out, tt.fix)
			assert.Contains(t, out, "See https://okedeji.io/hybernate/guides/gitops/")
		})
	}
}
