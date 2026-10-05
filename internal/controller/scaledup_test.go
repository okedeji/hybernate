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

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/autoscaler"
)

// scaledBy is the target as the API server returns it after manager last
// set its replicas.
func scaledBy(manager string, replicas int32) *appsv1.Deployment {
	d := targetDeploymentWithReplicas("api", "default", replicas)
	d.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate,
		Time: &metav1.Time{Time: fixedTime.Add(-time.Minute)}, FieldsType: "FieldsV1",
		FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:replicas":{}}}`)}}}
	return d
}

func drainEvents(t *testing.T, r *Reconciler) string {
	t.Helper()
	recorder, ok := r.Recorder.(*events.FakeRecorder)
	require.True(t, ok)
	var got []string
	for {
		select {
		case e := <-recorder.Events:
			got = append(got, e)
		default:
			return strings.Join(got, "\n")
		}
	}
}

// Whoever scales up a paused workload wants it running, so it's woken.
func TestWakeOnScaleUp_ByAPerson(t *testing.T) {
	workload := pausedWorkload(v1alpha1.PhasePaused)
	r := newTestReconcilerWithReplicas(t, workload, &stubPauser{}, 5)
	w := getWorkload(t, r, "api")

	require.NoError(t, r.wakeOnScaleUp(context.Background(), w, scaledBy("kubectl-scale", 5)))

	w = getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseRunning, w.Status.Phase)
	assert.Nil(t, w.Status.Pause)
	require.NotNil(t, w.Status.LastScaledUp)
	assert.Equal(t, "kubectl-scale", w.Status.LastScaledUp.By)
	assert.Empty(t, w.Status.LastScaledUp.GitOps)
	assert.Equal(t, int32(5), w.Status.LastScaledUp.Replicas)
	assert.Equal(t, v1alpha1.ActivitySourceScaledUp, w.Status.Activity.LastActivitySource, "the idle clock starts again")
	assert.Nil(t, meta.FindStatusCondition(w.Status.Conditions, conditionGitOpsConflict))
	assert.Equal(t, int32(5), targetReplicas(t, r), "left as they set it")
	assert.Contains(t, drainEvents(t, r), "scaled up to 5 replicas by kubectl-scale outside Hybernate")
}

// A GitOps tool undoing a pause is a setup problem: reported once with the
// fix, not fought.
func TestWakeOnScaleUp_ByGitOps(t *testing.T) {
	tests := []struct {
		manager, tool, fix string
	}{
		{"argocd-controller", "Argo CD", "jsonPointers: [/spec/replicas]"},
		{"kustomize-controller", "Flux", "Leave replicas out of the workload's manifest"},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			r := newTestReconcilerWithReplicas(t, pausedWorkload(v1alpha1.PhasePaused), &stubPauser{}, 3)
			w := getWorkload(t, r, "api")

			require.NoError(t, r.wakeOnScaleUp(context.Background(), w, scaledBy(tt.manager, 3)))

			w = getWorkload(t, r, "api")
			assert.Equal(t, v1alpha1.PhaseRunning, w.Status.Phase)
			assert.Equal(t, tt.tool, w.Status.LastScaledUp.GitOps)
			c := meta.FindStatusCondition(w.Status.Conditions, conditionGitOpsConflict)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionTrue, c.Status)
			assert.Contains(t, c.Message, tt.tool+" set the replicas from Git, undoing the pause")
			assert.Contains(t, c.Message, "pauses it again from 13:00 UTC")
			assert.Contains(t, strings.Join(strings.Fields(c.Message), " "), tt.fix)
			assert.Contains(t, drainEvents(t, r), "Warning GitOpsConflict")
		})
	}
}

func conflicted(scaledUp time.Time) *v1alpha1.ManagedWorkload {
	w := pausedWorkload(v1alpha1.PhaseIdle)
	w.Status.Pause = nil
	w.Status.LastScaledUp = &v1alpha1.ScaledUp{At: metav1.NewTime(scaledUp), By: "argocd-controller",
		GitOps: "Argo CD", Replicas: 3}
	meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{Type: conditionGitOpsConflict,
		Status: metav1.ConditionTrue, Reason: "PauseUndone"})
	return w
}

// After a GitOps tool undoes a pause, Hybernate waits before pausing again,
// so it doesn't restart the workload in a loop with the tool.
func TestHandlePause_WaitsAfterAGitOpsConflict(t *testing.T) {
	tests := []struct {
		name      string
		scaledUp  time.Duration
		wantPause bool
	}{
		{name: "within the hour", scaledUp: 10 * time.Minute},
		{name: "after it", scaledUp: 61 * time.Minute, wantPause: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pauser := &stubPauser{}
			r := newTestReconcilerWithReplicas(t, conflicted(fixedTime.Add(-tt.scaledUp)), pauser, 3)
			w := getWorkload(t, r, "api")

			result, err := r.handlePause(context.Background(), w, nil)

			require.NoError(t, err)
			assert.Equal(t, tt.wantPause, pauser.pauseCalls > 0)
			if !tt.wantPause {
				require.NotNil(t, result)
				assert.Equal(t, 50*time.Minute, result.RequeueAfter, "until an hour after the conflict")
				assert.Equal(t, v1alpha1.PhaseIdle, getWorkload(t, r, "api").Status.Phase)
			}
		})
	}
}

// A pause that holds shows the tool now leaves the replicas alone.
func TestGitOpsConflict_ClearsWhenAPauseHolds(t *testing.T) {
	t.Run("for an hour", func(t *testing.T) {
		workload := conflicted(fixedTime.Add(-3 * time.Hour))
		workload.Status.Phase = v1alpha1.PhasePaused
		workload.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 3,
			PausedAt: &metav1.Time{Time: fixedTime.Add(-61 * time.Minute)}}
		r := newTestReconcilerWithReplicas(t, workload, &stubPauser{}, 0)

		_, err := r.Reconcile(context.Background(), reconcileFor("api"))
		require.NoError(t, err)

		assert.Nil(t, meta.FindStatusCondition(getWorkload(t, r, "api").Status.Conditions, conditionGitOpsConflict))
		assert.Contains(t, drainEvents(t, r), "a pause held, so Argo CD leaves the replicas to Hybernate now")
	})
	t.Run("not yet", func(t *testing.T) {
		workload := conflicted(fixedTime.Add(-3 * time.Hour))
		workload.Status.Phase = v1alpha1.PhasePaused
		workload.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 3,
			PausedAt: &metav1.Time{Time: fixedTime.Add(-10 * time.Minute)}}
		r := newTestReconcilerWithReplicas(t, workload, &stubPauser{}, 0)

		_, err := r.Reconcile(context.Background(), reconcileFor("api"))
		require.NoError(t, err)

		assert.NotNil(t, meta.FindStatusCondition(getWorkload(t, r, "api").Status.Conditions, conditionGitOpsConflict))
	})
	t.Run("until Hybernate woke it", func(t *testing.T) {
		workload := conflicted(fixedTime.Add(-3 * time.Hour))
		workload.Status.Phase = v1alpha1.PhasePaused
		workload.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 3,
			PausedAt: &metav1.Time{Time: fixedTime.Add(-10 * time.Minute)}}
		r := newTestReconcilerWithReplicas(t, workload, &stubPauser{resumeDone: true}, 0)
		w := getWorkload(t, r, "api")

		_, err := r.handleResume(context.Background(), w, nil)
		require.NoError(t, err)

		assert.Nil(t, meta.FindStatusCondition(getWorkload(t, r, "api").Status.Conditions, conditionGitOpsConflict))
	})
}

// A workload an autoscaler scales says so, and how Hybernate pauses it
// alongside.
func TestReportAutoscaler(t *testing.T) {
	scheme := testScheme(t)
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "api-hpa", Namespace: "default"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: ptr.To[int32](2), MaxReplicas: 6,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api"}},
	}
	for _, tt := range []struct {
		name string
		objs []client.Object
		want string
	}{
		{name: "an HPA", objs: []client.Object{hpa},
			want: "HPA api-hpa scales it between 2 and 6 replicas; Hybernate pauses it at zero, where the HPA stops scaling"},
		{name: "nothing"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objs...).Build()
			r := &Reconciler{Client: c, autoscalers: autoscaler.NewFinder(c)}
			w := pausedWorkload(v1alpha1.PhaseRunning)
			meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{Type: conditionAutoscaled,
				Status: metav1.ConditionTrue, Reason: "HPA"})

			require.NoError(t, r.reportAutoscaler(context.Background(), w))

			cond := meta.FindStatusCondition(w.Status.Conditions, conditionAutoscaled)
			if tt.want == "" {
				assert.Nil(t, cond, "removed once nothing scales it")
				return
			}
			require.NotNil(t, cond)
			assert.Contains(t, cond.Message, tt.want)
		})
	}
}

func pausedScaledObject(t *testing.T, r *Reconciler) (string, bool) {
	t.Helper()
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(kedaScaledObject)
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "api-scaler"}, so))
	v, ok := so.GetAnnotations()[autoscaler.PausedReplicasAnnotation]
	return v, ok
}

// Someone scaling up a paused KEDA workload wakes it, and KEDA must be let
// go too, or it holds the workload at zero again and never scales it.
func TestWakeOnScaleUp_ReleasesTheKEDAHold(t *testing.T) {
	so := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"scaleTargetRef": map[string]any{"name": "api"}}}}
	so.SetGroupVersionKind(kedaScaledObject)
	so.SetNamespace("default")
	so.SetName("api-scaler")
	so.SetAnnotations(map[string]string{autoscaler.PausedReplicasAnnotation: "0"})
	workload := pausedWorkload(v1alpha1.PhasePaused)
	workload.Status.Pause.ScaledObject = "api-scaler"
	r := lifecycleReconciler(t, workload, 2, interceptor.Funcs{}, so)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	assert.Equal(t, v1alpha1.PhaseRunning, getWorkload(t, r, "api").Status.Phase)
	_, held := pausedScaledObject(t, r)
	assert.False(t, held, "KEDA scales it again")
	assert.Equal(t, int32(2), targetReplicas(t, r), "left as they set it")
}

// Right after a pause the cache can still show the replicas from before it.
// That is not a scale-up, and mustn't end the pause.
func TestWakeOnScaleUp_StaleCacheIsNotAScaleUp(t *testing.T) {
	workload := pausedWorkload(v1alpha1.PhasePaused)
	stale := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
		obj client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		if d, ok := obj.(*appsv1.Deployment); ok {
			d.Spec.Replicas = ptr.To(int32(3))
		}
		return nil
	}}
	r := lifecycleReconciler(t, workload, 0, stale)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	got := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhasePaused, got.Status.Phase)
	require.NotNil(t, got.Status.Pause)
	assert.Equal(t, int32(3), got.Status.Pause.PreviousReplicas)
	assert.Nil(t, got.Status.LastScaledUp)
}

// A workload scaled to zero outside Hybernate is meant to be off. Hybernate
// neither pauses it, which would record zero replicas to restore, nor ever
// starts it: not on idle, not on desiredState, not on a later wake.
func TestScaledToZero_IsLeftOff(t *testing.T) {
	idleLongAgo := &v1alpha1.ActivityStatus{LastActivityTime: metav1.NewTime(fixedTime.Add(-2 * time.Hour)),
		LastEvaluatedTime: ptr.To(metav1.NewTime(fixedTime.Add(-30 * time.Second)))}
	idlePolicy := &v1alpha1.IdlePolicySpec{IdleAfter: &metav1.Duration{Duration: time.Hour}}
	tests := []struct {
		name    string
		phase   v1alpha1.WorkloadPhase
		desired *v1alpha1.DesiredState
	}{
		{name: "idle for longer than idleAfter", phase: v1alpha1.PhaseRunning},
		{name: "already found idle", phase: v1alpha1.PhaseIdle},
		{name: "desiredState Paused", phase: v1alpha1.PhaseRunning, desired: desiredState(v1alpha1.DesiredStatePaused)},
		{name: "desiredState Running", phase: v1alpha1.PhaseRunning, desired: desiredState(v1alpha1.DesiredStateRunning)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := lifecycleWorkload("api", tt.desired, tt.phase)
			workload.Spec.IdlePolicy = idlePolicy
			workload.Status.Activity = idleLongAgo.DeepCopy()
			r := lifecycleReconciler(t, workload, 0, interceptor.Funcs{})

			reconcileUntilSettled(t, r, "api")

			got := getWorkload(t, r, "api")
			assert.Equal(t, v1alpha1.PhaseRunning, got.Status.Phase)
			assert.Nil(t, got.Status.Pause, "nothing to pause")
			assert.Equal(t, int32(0), targetReplicas(t, r))
			assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, conditionScaledToZero))
			events := drainEvents(t, r)
			assert.Equal(t, 1, strings.Count(events, ReasonScaledToZero), events)
			assert.NotContains(t, events, ReasonPaused)
		})
	}
}

// A cache that still shows the replicas a target had before someone scaled
// it to zero mustn't make Hybernate pause it with zero to restore.
func TestScaledToZero_StaleCacheDoesntPause(t *testing.T) {
	stale := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
		obj client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		if d, ok := obj.(*appsv1.Deployment); ok {
			d.Spec.Replicas = ptr.To(int32(3))
		}
		return nil
	}}
	workload := lifecycleWorkload("api", desiredState(v1alpha1.DesiredStatePaused), v1alpha1.PhaseRunning)
	r := lifecycleReconciler(t, workload, 0, stale)

	reconcileUntilSettled(t, r, "api")

	got := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseRunning, got.Status.Phase)
	assert.Nil(t, got.Status.Pause)
}

// Scaled back up, the workload is managed again, starting with a fresh
// idle clock rather than one that ran out while it was off.
func TestScaledToZero_BackUpIsActivity(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhaseRunning)
	workload.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{IdleAfter: &metav1.Duration{Duration: time.Hour}}
	workload.Status.Activity = &v1alpha1.ActivityStatus{LastActivityTime: metav1.NewTime(fixedTime.Add(-2 * time.Hour)),
		LastEvaluatedTime: ptr.To(metav1.NewTime(fixedTime.Add(-30 * time.Second)))}
	meta.SetStatusCondition(&workload.Status.Conditions, metav1.Condition{Type: conditionScaledToZero,
		Status: metav1.ConditionTrue, Reason: "ScaledToZero"})
	r := lifecycleReconciler(t, workload, 2, interceptor.Funcs{})

	reconcileUntilSettled(t, r, "api")

	got := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseRunning, got.Status.Phase)
	assert.Equal(t, int32(2), targetReplicas(t, r))
	assert.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, conditionScaledToZero))
	assert.Equal(t, v1alpha1.ActivitySourceScaledUp, got.Status.Activity.LastActivitySource)
}

// A pause recorded with zero replicas, as an earlier version made of a
// workload already at zero, is handed back at zero, and the event says so.
func TestHandBack_KeepsAWorkloadThatWasAtZero(t *testing.T) {
	tests := []struct {
		previous  int32
		want      int32
		wantEvent string
	}{
		{previous: 0, want: 0, wantEvent: "released at zero replicas: no longer managed"},
		{previous: 3, want: 3, wantEvent: "restored to 3 replicas: no longer managed"},
	}
	for _, tt := range tests {
		t.Run(tt.wantEvent, func(t *testing.T) {
			workload := pausedWorkload(v1alpha1.PhasePaused)
			workload.Finalizers = []string{finalizerName}
			workload.Status.Pause.PreviousReplicas = tt.previous
			r := realPauserReconciler(t, workload)
			require.NoError(t, r.Delete(context.Background(), getWorkload(t, r, "api")))

			_, err := r.Reconcile(context.Background(), reconcileFor("api"))
			require.NoError(t, err)

			assert.Equal(t, tt.want, targetReplicas(t, r))
			assert.Contains(t, drainEvents(t, r), tt.wantEvent)
		})
	}
}

// A resume of Hybernate's own scales the workload up too. The cache can
// still hold the workload as Paused once that has begun, which mustn't be
// taken for someone else waking it: that would release KEDA, which could
// scale the workload back down while it starts.
func TestWakeOnScaleUp_StaleCacheDuringAResume(t *testing.T) {
	pauser := &stubPauser{}
	r := newTestReconcilerWithReplicas(t, pausedWorkload(v1alpha1.PhaseResuming), pauser, 3)
	stale := getWorkload(t, r, "api")
	stale.Status.Phase = v1alpha1.PhasePaused

	require.NoError(t, r.wakeOnScaleUp(context.Background(), stale, scaledBy("hybernate", 3)))

	assert.Zero(t, pauser.restoreCalls, "KEDA is still held")
	got := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseResuming, got.Status.Phase)
	assert.Nil(t, got.Status.LastScaledUp)
}
