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
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/autoscaler"
	"github.com/okedeji/hybernate/internal/lifecycle"
	"github.com/okedeji/hybernate/internal/metrics"
)

var fixedTime = time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)

type stubPauser struct {
	pauseDone    bool
	pauseErr     error
	resumeDone   bool
	resumeErr    error
	pauseCalls   int
	resumeCalls  int
	restoreCalls int
}

func (s *stubPauser) Prepare(_ context.Context, workload *v1alpha1.ManagedWorkload) error {
	workload.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1}
	return nil
}

func (s *stubPauser) Pause(_ context.Context, _ *v1alpha1.ManagedWorkload) (bool, error) {
	s.pauseCalls++
	return s.pauseDone, s.pauseErr
}

func (s *stubPauser) Resume(_ context.Context, _ *v1alpha1.ManagedWorkload) (bool, error) {
	s.resumeCalls++
	return s.resumeDone, s.resumeErr
}

func (s *stubPauser) Restore(_ context.Context, w *v1alpha1.ManagedWorkload) (int32, error) {
	s.restoreCalls++
	return w.Status.Pause.PreviousReplicas, nil
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
	require.NoError(t, autoscalingv2.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, discoveryv1.AddToScheme(s))
	return s
}

func targetDeployment(name, namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

func targetDeploymentWithReplicas(name, namespace string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

func newTestReconcilerWithReplicas(t *testing.T, workload *v1alpha1.ManagedWorkload, pauser *stubPauser, replicas int32) *Reconciler {
	t.Helper()
	scheme := testScheme(t)

	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.ManagedWorkload{})
	if workload != nil {
		builder = builder.WithObjects(workload)
		builder = builder.WithObjects(targetDeploymentWithReplicas(workload.Spec.Target.Name, workload.Namespace, replicas))
	}

	return &Reconciler{
		Client:   builder.Build(),
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(10),
		pauser:   pauser,
		engines:  newEngineRegistry(func() forecaster { return &stubForecaster{} }),
		clock:    func() time.Time { return fixedTime },
	}
}

func newTestReconciler(t *testing.T, workload *v1alpha1.ManagedWorkload, pauser *stubPauser) *Reconciler {
	t.Helper()
	return newTestReconcilerWithTarget(t, workload, pauser, true)
}

func newTestReconcilerWithTarget(t *testing.T, workload *v1alpha1.ManagedWorkload, pauser *stubPauser, createTarget bool) *Reconciler {
	t.Helper()
	scheme := testScheme(t)

	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.ManagedWorkload{})
	if workload != nil {
		builder = builder.WithObjects(workload)
		if createTarget {
			target := targetDeployment(workload.Spec.Target.Name, workload.Namespace)
			// A workload Hybernate has paused is at zero; any more would be
			// a scale-up outside it, which wakes it.
			if workload.Status.Phase == v1alpha1.PhasePaused {
				target = targetDeploymentWithReplicas(workload.Spec.Target.Name, workload.Namespace, 0)
			}
			builder = builder.WithObjects(target)
		}
	}

	return &Reconciler{
		Client:   builder.Build(),
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(10),
		pauser:   pauser,
		engines:  newEngineRegistry(func() forecaster { return &stubForecaster{} }),
		clock:    func() time.Time { return fixedTime },
	}
}

func reconcileFor(name string) reconcile.Request {
	return reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
	}
}

func getWorkload(t *testing.T, r *Reconciler, name string) *v1alpha1.ManagedWorkload {
	t.Helper()
	var w v1alpha1.ManagedWorkload
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &w))
	return &w
}

func TestReconcile_SetsInitialPhaseToRunning(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
	}

	r := newTestReconciler(t, workload, &stubPauser{})
	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	w := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseRunning, w.Status.Phase)
	assert.True(t, len(w.Finalizers) > 0, "finalizer should be set")
}

func TestReconcile_NotFoundIsNoOp(t *testing.T) {
	r := newTestReconciler(t, nil, &stubPauser{})
	result, err := r.Reconcile(context.Background(), reconcileFor("missing"))
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestReconcile_PauseTransitions(t *testing.T) {
	workload := withPauseRequested(lifecycleWorkload("api", v1alpha1.PhaseRunning))

	pauser := &stubPauser{pauseDone: true}
	r := newTestReconciler(t, workload, pauser)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	w := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhasePaused, w.Status.Phase)
	assert.Equal(t, 1, pauser.pauseCalls)
}

func TestReconcile_PauseNotDoneRequeues(t *testing.T) {
	workload := withPauseRequested(lifecycleWorkload("api", v1alpha1.PhaseRunning))

	pauser := &stubPauser{pauseDone: false}
	r := newTestReconciler(t, workload, pauser)

	result, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, result.RequeueAfter)
}

func TestReconcile_AlreadyPausedIsNoOp(t *testing.T) {
	workload := withPauseRequested(lifecycleWorkload("api", v1alpha1.PhasePaused))

	pauser := &stubPauser{}
	r := newTestReconciler(t, workload, pauser)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	assert.Equal(t, 0, pauser.pauseCalls)
}

func TestReconcile_ResumeTransitions(t *testing.T) {
	workload := wokenByActivity(lifecycleWorkload("api", v1alpha1.PhasePaused))

	pauser := &stubPauser{resumeDone: true}
	r := newTestReconciler(t, workload, pauser)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	w := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseRunning, w.Status.Phase)
	assert.Equal(t, 1, pauser.resumeCalls)
}

func TestReconcile_ResumeNotReadyRequeues(t *testing.T) {
	workload := wokenByActivity(lifecycleWorkload("api", v1alpha1.PhasePaused))

	pauser := &stubPauser{resumeDone: false}
	r := newTestReconciler(t, workload, pauser)

	result, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, result.RequeueAfter)
}

// Pods that never become Ready back the resume's retries off, from 5s to a
// minute, rather than annotating, listing and scaling every 5s forever.
func TestReconcile_ResumeNotReadyBacksOff(t *testing.T) {
	tests := []struct {
		waited, want time.Duration
	}{
		{waited: 0, want: 5 * time.Second},
		{waited: 20 * time.Second, want: 20 * time.Second},
		{waited: time.Hour, want: time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.waited.String(), func(t *testing.T) {
			workload := pausedWorkload(v1alpha1.PhaseResuming)
			workload.Status.LastTransitionTime = ptr.To(metav1.NewTime(fixedTime.Add(-tt.waited)))
			pauser := &stubPauser{resumeDone: false}
			r := newTestReconcilerWithReplicas(t, workload, pauser, 3)

			result, err := r.Reconcile(context.Background(), reconcileFor("api"))

			require.NoError(t, err)
			assert.Equal(t, 1, pauser.resumeCalls)
			assert.Equal(t, tt.want, result.RequeueAfter)
		})
	}
}

func TestReconcile_FinalizerAddedOnFirstReconcile(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
	}

	r := newTestReconciler(t, workload, &stubPauser{})
	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	w := getWorkload(t, r, "api")
	assert.Contains(t, w.Finalizers, finalizerName)
}

func TestReconcile_DeletionRemovesFinalizer(t *testing.T) {
	now := metav1.Now()
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "api",
			Namespace:         "default",
			Finalizers:        []string{finalizerName},
			DeletionTimestamp: &now,
		},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning},
	}

	r := newTestReconciler(t, workload, &stubPauser{})
	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	// Object is fully deleted once finalizer is removed by the fake client.
	var w v1alpha1.ManagedWorkload
	err = r.Get(context.Background(), types.NamespacedName{Name: "api", Namespace: "default"}, &w)
	assert.True(t, err != nil, "object should be deleted after finalizer removal")
}

// No longer managing a workload must never leave it switched off: deleting
// the ManagedWorkload of a paused one restores the replicas it had.
func TestReconcileDelete_RestoresAPausedWorkload(t *testing.T) {
	tests := []struct {
		name         string
		phase        v1alpha1.WorkloadPhase
		pause        *v1alpha1.PauseStatus
		withTarget   bool
		wantReplicas int32
	}{
		{name: "paused", phase: v1alpha1.PhasePaused, pause: &v1alpha1.PauseStatus{PreviousReplicas: 3},
			withTarget: true, wantReplicas: 3},
		{name: "running", phase: v1alpha1.PhaseRunning, withTarget: true, wantReplicas: 0},
		{name: "paused, but its Deployment is gone", phase: v1alpha1.PhasePaused,
			pause: &v1alpha1.PauseStatus{PreviousReplicas: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := metav1.Now()
			workload := &v1alpha1.ManagedWorkload{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default", Finalizers: []string{finalizerName},
					DeletionTimestamp: &now},
				Spec:   v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}},
				Status: v1alpha1.ManagedWorkloadStatus{Phase: tt.phase, Pause: tt.pause},
			}
			builder := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(workload)
			if tt.withTarget {
				builder = builder.WithObjects(targetDeploymentWithReplicas("api", "default", 0))
			}
			c := builder.Build()
			r := &Reconciler{Client: c, Scheme: testScheme(t), Recorder: events.NewFakeRecorder(10),
				pauser:  lifecycle.NewPauser(c, autoscaler.NewFinder(c)),
				engines: newEngineRegistry(func() forecaster { return &stubForecaster{} }),
				clock:   func() time.Time { return fixedTime }}

			_, err := r.Reconcile(context.Background(), reconcileFor("api"))
			require.NoError(t, err)

			var w v1alpha1.ManagedWorkload
			assert.True(t, apierrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Name: "api", Namespace: "default"}, &w)),
				"the deletion still completes")
			if tt.withTarget {
				var d appsv1.Deployment
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "api", Namespace: "default"}, &d))
				assert.Equal(t, tt.wantReplicas, *d.Spec.Replicas)
			}
		})
	}
}

func TestReconcile_TargetNotFoundSetsConditionAndRequeues(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning},
	}

	r := newTestReconcilerWithTarget(t, workload, &stubPauser{}, false)

	result, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	assert.Equal(t, 1*time.Minute, result.RequeueAfter)

	w := getWorkload(t, r, "api")
	var found bool
	for _, c := range w.Status.Conditions {
		if c.Type == conditionTargetAvailable {
			assert.Equal(t, metav1.ConditionFalse, c.Status)
			assert.Equal(t, "TargetNotFound", c.Reason)
			found = true
		}
	}
	assert.True(t, found, "TargetAvailable condition should be set")
}

func pausedWorkload(phase v1alpha1.WorkloadPhase) *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase: phase,
			Pause: &v1alpha1.PauseStatus{PreviousReplicas: 3},
		},
	}
}

func targetReplicas(t *testing.T, r *Reconciler) int32 {
	t.Helper()
	var dep appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "api", Namespace: "default"}, &dep))
	return *dep.Spec.Replicas
}

// Nothing pauses a running workload but Hybernate, so a running workload's
// replica changes are its team's or an autoscaler's, and left alone.
func TestReconcile_RunningReplicaChangesAreLeftAlone(t *testing.T) {
	workload := pausedWorkload(v1alpha1.PhaseRunning)
	workload.Status.Pause = nil
	r := newTestReconcilerWithReplicas(t, workload, &stubPauser{}, 7)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	assert.Equal(t, int32(7), targetReplicas(t, r))
	assert.Nil(t, getWorkload(t, r, "api").Status.LastScaledUp)
}

func TestReconcile_PausedAtZeroStaysPaused(t *testing.T) {
	r := newTestReconcilerWithReplicas(t, pausedWorkload(v1alpha1.PhasePaused), &stubPauser{}, 0)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	w := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhasePaused, w.Status.Phase)
	assert.Nil(t, w.Status.LastScaledUp)
}

func TestReconcile_NothingAskedIsNoOp(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning},
	}

	pauser := &stubPauser{}
	r := newTestReconciler(t, workload, pauser)

	result, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	// Automation requeues for the forecast to go on learning.
	assert.Equal(t, activityCheckInterval, result.RequeueAfter)
	assert.Equal(t, 0, pauser.pauseCalls)
	assert.Equal(t, 0, pauser.resumeCalls)
}

func TestReconcile_DuplicateTargetBlocksNewer(t *testing.T) {
	older := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "deployment-idle-app",
			Namespace:         "default",
			UID:               "aaa",
			CreationTimestamp: metav1.NewTime(fixedTime.Add(-1 * time.Hour)),
			ResourceVersion:   "1",
		},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:     v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "idle-app"},
			Prediction: v1alpha1.PredictionSpec{Confidence: 85},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase: v1alpha1.PhaseRunning,
		},
	}

	newer := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "duplicate-idle-app",
			Namespace:         "default",
			UID:               "bbb",
			CreationTimestamp: metav1.NewTime(fixedTime),
			ResourceVersion:   "2",
		},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:     v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "idle-app"},
			Prediction: v1alpha1.PredictionSpec{Confidence: 85},
		},
	}

	scheme := testScheme(t)
	recorder := events.NewFakeRecorder(10)
	k := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.ManagedWorkload{}).
		WithObjects(older, newer, targetDeployment("idle-app", "default")).
		Build()

	r := &Reconciler{
		Client:   k,
		Scheme:   scheme,
		Recorder: recorder,
		pauser:   &stubPauser{},
		engines:  newEngineRegistry(func() forecaster { return &stubForecaster{} }),
		clock:    func() time.Time { return fixedTime },
	}

	// Reconcile the newer one — should be blocked.
	_, err := r.Reconcile(context.Background(), reconcileFor("duplicate-idle-app"))
	require.NoError(t, err)

	w := getWorkload(t, r, "duplicate-idle-app")
	require.NotEmpty(t, w.Status.Conditions)

	var found bool
	for _, c := range w.Status.Conditions {
		if c.Type == "DuplicateTarget" {
			found = true
			assert.Equal(t, metav1.ConditionTrue, c.Status)
			assert.Contains(t, c.Message, "deployment-idle-app")
			break
		}
	}
	require.True(t, found, "expected DuplicateTarget condition")

	// Verify warning event was emitted.
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "DuplicateTarget")
	default:
		t.Fatal("expected DuplicateTarget warning event")
	}
}

func TestReconcile_DuplicateTargetAllowsOlder(t *testing.T) {
	older := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "deployment-idle-app",
			Namespace:         "default",
			UID:               "aaa",
			CreationTimestamp: metav1.NewTime(fixedTime.Add(-1 * time.Hour)),
			ResourceVersion:   "1",
		},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:     v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "idle-app"},
			Prediction: v1alpha1.PredictionSpec{Confidence: 85},
		},
	}

	newer := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "duplicate-idle-app",
			Namespace:         "default",
			UID:               "bbb",
			CreationTimestamp: metav1.NewTime(fixedTime),
			ResourceVersion:   "2",
		},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:     v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "idle-app"},
			Prediction: v1alpha1.PredictionSpec{Confidence: 85},
		},
	}

	scheme := testScheme(t)
	k := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.ManagedWorkload{}).
		WithObjects(older, newer, targetDeployment("idle-app", "default")).
		Build()

	r := &Reconciler{
		Client:   k,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(10),
		pauser:   &stubPauser{},
		engines:  newEngineRegistry(func() forecaster { return &stubForecaster{} }),
		clock:    func() time.Time { return fixedTime },
	}

	// Reconcile the older one — should proceed normally.
	_, err := r.Reconcile(context.Background(), reconcileFor("deployment-idle-app"))
	require.NoError(t, err)

	w := getWorkload(t, r, "deployment-idle-app")
	for _, c := range w.Status.Conditions {
		assert.NotEqual(t, "DuplicateTarget", c.Type, "older workload should not get DuplicateTarget")
	}
	assert.Equal(t, v1alpha1.PhaseRunning, w.Status.Phase)
}

func TestReconcile_DuplicateTargetClearsWhenResolved(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "deployment-idle-app",
			Namespace:         "default",
			UID:               "aaa",
			CreationTimestamp: metav1.NewTime(fixedTime),
			ResourceVersion:   "1",
		},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:     v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "idle-app"},
			Prediction: v1alpha1.PredictionSpec{Confidence: 85},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase: v1alpha1.PhaseRunning,
			Conditions: []metav1.Condition{
				{
					Type:               "DuplicateTarget",
					Status:             metav1.ConditionTrue,
					Reason:             "DuplicateTarget",
					Message:            "was duplicate",
					LastTransitionTime: metav1.NewTime(fixedTime),
				},
			},
		},
	}

	scheme := testScheme(t)
	k := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.ManagedWorkload{}).
		WithObjects(workload, targetDeployment("idle-app", "default")).
		Build()

	r := &Reconciler{
		Client:   k,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(10),
		pauser:   &stubPauser{},
		engines:  newEngineRegistry(func() forecaster { return &stubForecaster{} }),
		clock:    func() time.Time { return fixedTime },
	}

	// No duplicate exists anymore — condition should clear.
	_, err := r.Reconcile(context.Background(), reconcileFor("deployment-idle-app"))
	require.NoError(t, err)

	w := getWorkload(t, r, "deployment-idle-app")
	for _, c := range w.Status.Conditions {
		if c.Type == "DuplicateTarget" {
			assert.Equal(t, metav1.ConditionFalse, c.Status, "condition should be cleared")
		}
	}
}

func sharedTargetWorkload(name string, uid types.UID, created time.Time) *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			UID:               uid,
			CreationTimestamp: metav1.NewTime(created),
			Finalizers:        []string{finalizerName},
		},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:     v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "idle-app"},
			Prediction: v1alpha1.PredictionSpec{Confidence: 85},
		},
	}
}

func newSharedTargetReconciler(t *testing.T, objs ...client.Object) (*Reconciler, *events.FakeRecorder) {
	t.Helper()
	scheme := testScheme(t)
	recorder := events.NewFakeRecorder(10)
	k := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.ManagedWorkload{}).
		WithObjects(append(objs, targetDeployment("idle-app", "default"))...).
		Build()
	return &Reconciler{
		Client:   k,
		Scheme:   scheme,
		Recorder: recorder,
		pauser:   &stubPauser{},
		engines:  newEngineRegistry(func() forecaster { return &stubForecaster{} }),
		clock:    func() time.Time { return fixedTime },
	}, recorder
}

func isDuplicate(t *testing.T, r *Reconciler, name string) bool {
	t.Helper()
	return meta.IsStatusConditionTrue(getWorkload(t, r, name).Status.Conditions, conditionDuplicateTarget)
}

func TestReconcile_DuplicateTargetExactlyOneOwner(t *testing.T) {
	tests := []struct {
		name      string
		olderUID  types.UID
		newerUID  types.UID
		sameTime  bool
		wantOwner string
	}{
		{name: "older has smaller UID", olderUID: "aaa", newerUID: "zzz", wantOwner: "older"},
		{name: "older has larger UID", olderUID: "zzz", newerUID: "aaa", wantOwner: "older"},
		{name: "same second, smaller UID wins", olderUID: "zzz", newerUID: "aaa", sameTime: true, wantOwner: "newer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newerCreated := fixedTime
			if tt.sameTime {
				newerCreated = fixedTime.Add(-1 * time.Hour)
			}
			older := sharedTargetWorkload("older", tt.olderUID, fixedTime.Add(-1*time.Hour))
			newer := sharedTargetWorkload("newer", tt.newerUID, newerCreated)
			r, _ := newSharedTargetReconciler(t, older, newer)

			for _, name := range []string{"older", "newer"} {
				_, err := r.Reconcile(context.Background(), reconcileFor(name))
				require.NoError(t, err)
			}

			assert.NotEqual(t, isDuplicate(t, r, "older"), isDuplicate(t, r, "newer"),
				"exactly one workload must own the target")
			assert.False(t, isDuplicate(t, r, tt.wantOwner), "%s should own the target", tt.wantOwner)
		})
	}
}

func TestReconcile_DuplicateTakesOverWhenOwnerIsDeleting(t *testing.T) {
	owner := sharedTargetWorkload("older", "aaa", fixedTime.Add(-1*time.Hour))
	deleting := metav1.NewTime(fixedTime)
	owner.DeletionTimestamp = &deleting
	duplicate := sharedTargetWorkload("newer", "bbb", fixedTime)
	r, _ := newSharedTargetReconciler(t, owner, duplicate)

	_, err := r.Reconcile(context.Background(), reconcileFor("newer"))
	require.NoError(t, err)

	assert.False(t, isDuplicate(t, r, "newer"))
}

func TestReconcile_DuplicateRequeuesAndWarnsOnce(t *testing.T) {
	owner := sharedTargetWorkload("older", "aaa", fixedTime.Add(-1*time.Hour))
	duplicate := sharedTargetWorkload("newer", "bbb", fixedTime)
	r, recorder := newSharedTargetReconciler(t, owner, duplicate)

	for range 2 {
		result, err := r.Reconcile(context.Background(), reconcileFor("newer"))
		require.NoError(t, err)
		assert.Equal(t, duplicateRecheckInterval, result.RequeueAfter)
	}

	assert.Len(t, recorder.Events, 1, "duplicate warning should only fire when first detected")
}

func TestFindRelatedWorkloads_SharingTarget(t *testing.T) {
	owner := sharedTargetWorkload("older", "aaa", fixedTime.Add(-1*time.Hour))
	duplicate := sharedTargetWorkload("newer", "bbb", fixedTime)
	unrelated := sharedTargetWorkload("other", "ccc", fixedTime)
	unrelated.Spec.Target.Name = "other-app"
	r, _ := newSharedTargetReconciler(t, owner, duplicate, unrelated)

	requests := r.findRelatedWorkloads(context.Background(), owner)

	assert.Equal(t, []reconcile.Request{reconcileFor("newer")}, requests)
}

func lifecycleWorkload(name string, phase v1alpha1.WorkloadPhase) *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: name},
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: phase},
	}
}

// wokenByActivity stamps w with activity, as kubectl hybernate wake does,
// which wakes it if it's paused.
func wokenByActivity(w *v1alpha1.ManagedWorkload) *v1alpha1.ManagedWorkload {
	if w.Annotations == nil {
		w.Annotations = map[string]string{}
	}
	w.Annotations[v1alpha1.AnnotationLastActivity] = fixedTime.Format(time.RFC3339)
	return w
}

func TestReconcile_ResumesInterruptedTransition(t *testing.T) {
	tests := []struct {
		name      string
		phase     v1alpha1.WorkloadPhase
		wantPhase v1alpha1.WorkloadPhase
	}{
		{name: "pause", phase: v1alpha1.PhasePausing, wantPhase: v1alpha1.PhasePaused},
		{name: "resume", phase: v1alpha1.PhaseResuming, wantPhase: v1alpha1.PhaseRunning},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := lifecycleWorkload("stuck-app", tt.phase)
			pauser := &stubPauser{pauseDone: true, resumeDone: true}
			r := newTestReconciler(t, workload, pauser)

			_, err := r.Reconcile(context.Background(), reconcileFor("stuck-app"))
			require.NoError(t, err)

			assert.Equal(t, tt.wantPhase, getWorkload(t, r, "stuck-app").Status.Phase)
		})
	}
}

func TestSetCondition_UpdatesReasonWithoutStatusChange(t *testing.T) {
	r := &Reconciler{clock: func() time.Time { return fixedTime }}
	workload := &v1alpha1.ManagedWorkload{}

	r.setCondition(workload, conditionMetricsAvailable, metav1.ConditionFalse, "NoPodMetrics", "no pods reporting")
	r.clock = func() time.Time { return fixedTime.Add(time.Hour) }
	r.setCondition(workload, conditionMetricsAvailable, metav1.ConditionFalse, "MetricsUnavailable", "metrics API down")

	cond := meta.FindStatusCondition(workload.Status.Conditions, conditionMetricsAvailable)
	require.NotNil(t, cond)
	assert.Equal(t, "MetricsUnavailable", cond.Reason, "a new cause must replace the stale one")
	assert.Equal(t, "metrics API down", cond.Message)
	assert.True(t, cond.LastTransitionTime.Time.Equal(fixedTime), "transition time only moves when the status changes")
}

func TestFindWorkloadsForTarget_MatchesKindAndName(t *testing.T) {
	forDeployment := sharedTargetWorkload("deployment-api", "aaa", fixedTime)
	forDeployment.Spec.Target = v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}
	forStatefulSet := sharedTargetWorkload("statefulset-api", "bbb", fixedTime)
	forStatefulSet.Spec.Target = v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindStatefulSet, Name: "api"}
	r, _ := newSharedTargetReconciler(t, forDeployment, forStatefulSet)

	tests := []struct {
		name string
		obj  client.Object
		want []reconcile.Request
	}{
		{
			name: "deployment",
			obj:  &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}},
			want: []reconcile.Request{reconcileFor("deployment-api")},
		},
		{
			name: "statefulset with the same name",
			obj:  &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}},
			want: []reconcile.Request{reconcileFor("statefulset-api")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, r.findWorkloadsForTarget(context.Background(), tt.obj))
		})
	}
}

func TestReconcile_WorkloadPhaseGaugeFollowsTransitions(t *testing.T) {
	workload := withPauseRequested(lifecycleWorkload("phase-gauge-app", v1alpha1.PhaseRunning))
	r := newTestReconciler(t, workload, &stubPauser{pauseDone: true})

	_, err := r.Reconcile(context.Background(), reconcileFor("phase-gauge-app"))
	require.NoError(t, err)
	require.Equal(t, v1alpha1.PhasePaused, getWorkload(t, r, "phase-gauge-app").Status.Phase)

	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.WorkloadPhase.WithLabelValues("default", "phase-gauge-app", "Paused")))
	for _, stale := range []string{"Running", "Pausing"} {
		assert.False(t, metrics.WorkloadPhase.DeleteLabelValues("default", "phase-gauge-app", stale),
			"%s must not linger after the workload leaves it", stale)
	}
}

func TestReconcileDelete_DropsEveryWorkloadSeries(t *testing.T) {
	workload := lifecycleWorkload("deleted-gauge-app", v1alpha1.PhaseRunning)
	workload.Finalizers = []string{finalizerName}
	deleting := metav1.NewTime(fixedTime)
	workload.DeletionTimestamp = &deleting
	r := newTestReconciler(t, workload, &stubPauser{})
	metrics.WorkloadPhase.WithLabelValues("default", "deleted-gauge-app", "Running").Set(1)
	metrics.IdleSeconds.WithLabelValues("default", "deleted-gauge-app").Set(60)
	metrics.IdleDetections.WithLabelValues("default", "deleted-gauge-app").Inc()
	metrics.PredictionPhase.WithLabelValues("default", "deleted-gauge-app").Set(1)

	_, err := r.Reconcile(context.Background(), reconcileFor("deleted-gauge-app"))
	require.NoError(t, err)

	assert.False(t, metrics.WorkloadPhase.DeleteLabelValues("default", "deleted-gauge-app", "Running"),
		"a deleted workload must not leave a phase series behind")
	assert.False(t, metrics.IdleSeconds.DeleteLabelValues("default", "deleted-gauge-app"))
	assert.False(t, metrics.IdleDetections.DeleteLabelValues("default", "deleted-gauge-app"))
	assert.False(t, metrics.PredictionPhase.DeleteLabelValues("default", "deleted-gauge-app"))
}

const statusSubresource = "status"

var kedaScaledObject = schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"}

// lifecycleReconciler reconciles workload with the real lifecycle Pauser,
// against a fake API server that knows KEDA's kinds and holds the workload,
// its target at replicas, all of them Ready, and objs.
func lifecycleReconciler(t *testing.T, workload *v1alpha1.ManagedWorkload, replicas int32, funcs interceptor.Funcs,
	objs ...client.Object) *Reconciler {
	t.Helper()
	scheme := testScheme(t)
	mapper := meta.NewDefaultRESTMapper(nil)
	for gvk := range scheme.AllKnownTypes() {
		scope := meta.RESTScopeNamespace
		if gvk.Kind == "Namespace" || gvk.Kind == "Node" {
			scope = meta.RESTScopeRoot
		}
		mapper.Add(gvk, scope)
	}
	mapper.Add(kedaScaledObject, meta.RESTScopeNamespace)
	target := targetDeploymentWithReplicas(workload.Spec.Target.Name, workload.Namespace, replicas)
	target.Status.ReadyReplicas = 10
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).
		WithStatusSubresource(&v1alpha1.ManagedWorkload{}).
		WithObjects(append([]client.Object{workload, target}, objs...)...).
		WithInterceptorFuncs(funcs).Build()
	finder := autoscaler.NewFinder(c)
	return &Reconciler{
		Client:      c,
		Scheme:      scheme,
		Recorder:    events.NewFakeRecorder(100),
		pauser:      lifecycle.NewPauser(c, finder),
		autoscalers: finder,
		metrics:     &stubMetrics{cpuMillis: 5, cpuPerReplica: 500, memoryPerReplica: 1 << 30, replicas: max(replicas, 1)},
		engines:     newEngineRegistry(func() forecaster { return &stubForecaster{} }),
		clock:       func() time.Time { return fixedTime },
	}
}

func realPauserReconciler(t *testing.T, workload *v1alpha1.ManagedWorkload, objs ...client.Object) *Reconciler {
	t.Helper()
	return lifecycleReconciler(t, workload, 0, interceptor.Funcs{}, objs...)
}

// failStatusWrite fails the first status write of a ManagedWorkload that
// matches, the way a conflict with another writer does.
func failStatusWrite(matches func(*v1alpha1.ManagedWorkload) bool) interceptor.Funcs {
	failed := false
	return interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if mw, ok := obj.(*v1alpha1.ManagedWorkload); ok && sub == statusSubresource && !failed && matches(mw) {
				failed = true
				return apierrors.NewConflict(schema.GroupResource{Group: "hybernate.io", Resource: "managedworkloads"},
					mw.Name, errors.New("the object has been modified"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
}

func inPhase(phase v1alpha1.WorkloadPhase) func(*v1alpha1.ManagedWorkload) bool {
	return func(w *v1alpha1.ManagedWorkload) bool { return w.Status.Phase == phase }
}

// reconcileUntilSettled reconciles the workload named api until it settles.
func reconcileUntilSettled(t *testing.T, r *Reconciler) {
	t.Helper()
	for range 8 {
		_, _ = r.Reconcile(context.Background(), reconcileFor("api")) // an injected failure is retried, as the controller would
	}
}

// What a pause will change is written to the API before the workload is
// scaled to zero, so nothing that happens afterwards can lose it.
func TestPause_RecordIsWrittenBeforeScalingToZero(t *testing.T) {
	workload := withPauseRequested(lifecycleWorkload("api", v1alpha1.PhaseRunning))
	lastActivity := fixedTime.Add(-2 * time.Hour).Format(time.RFC3339)
	workload.Annotations[v1alpha1.AnnotationLastActivity] = lastActivity
	var recorded *v1alpha1.PauseStatus
	funcs := interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok && sub == "scale" && recorded == nil {
				var stored v1alpha1.ManagedWorkload
				require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "api"}, &stored))
				recorded = stored.Status.Pause
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	r := lifecycleReconciler(t, workload, 3, funcs)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	require.NotNil(t, recorded, "the pause must be recorded before the target is scaled")
	assert.Equal(t, int32(3), recorded.PreviousReplicas)
	assert.NotNil(t, recorded.Resources, "the savings snapshot is taken while the pods still run")
	require.NotNil(t, recorded.WakeAnnotations)
	assert.Equal(t, map[string]string{v1alpha1.AnnotationLastActivity: lastActivity}, recorded.WakeAnnotations.Workload,
		"so a later change wakes it whatever time it states")
}

// A pause interrupted between scaling to zero and recording Paused, by a
// conflict, a crash or a leader change, is finished from its record: the
// retry must not read the target, now at zero, as the count to restore.
func TestPause_InterruptedBeforePausedKeepsTheReplicaCount(t *testing.T) {
	workload := withPauseRequested(lifecycleWorkload("api", v1alpha1.PhaseRunning))
	r := lifecycleReconciler(t, workload, 3, failStatusWrite(inPhase(v1alpha1.PhasePaused)))

	res, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err, "a conflict is retried, not reported")
	require.Equal(t, staleRetry, res.RequeueAfter)
	interrupted := getWorkload(t, r, "api")
	require.Equal(t, v1alpha1.PhasePausing, interrupted.Status.Phase)
	require.Equal(t, int32(0), targetReplicas(t, r))

	_, err = r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	paused := getWorkload(t, r, "api")
	require.Equal(t, v1alpha1.PhasePaused, paused.Status.Phase)
	assert.Equal(t, int32(3), paused.Status.Pause.PreviousReplicas)
	require.NotNil(t, paused.Status.Pause.Resources)
	assert.Equal(t, int32(3), paused.Status.Pause.Resources.Replicas, "savings are priced at what ran, not at zero")

	require.NoError(t, r.Update(context.Background(), wokenByActivity(paused)))
	reconcileUntilSettled(t, r)
	assert.Equal(t, v1alpha1.PhaseRunning, getWorkload(t, r, "api").Status.Phase)
	assert.Equal(t, int32(3), targetReplicas(t, r))
}

// Deleting the ManagedWorkload of a pause that was interrupted still gives
// the workload back the replicas it had.
func TestDelete_DuringAnInterruptedPauseRestoresTheReplicas(t *testing.T) {
	workload := withPauseRequested(lifecycleWorkload("api", v1alpha1.PhaseRunning))
	r := lifecycleReconciler(t, workload, 3, failStatusWrite(inPhase(v1alpha1.PhasePaused)))
	res, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err, "a conflict is retried, not reported")
	require.Equal(t, staleRetry, res.RequeueAfter)
	require.Equal(t, int32(0), targetReplicas(t, r))

	require.NoError(t, r.Delete(context.Background(), getWorkload(t, r, "api")))
	_, err = r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	var w v1alpha1.ManagedWorkload
	assert.True(t, apierrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Name: "api", Namespace: "default"}, &w)))
	assert.Equal(t, int32(3), targetReplicas(t, r))
}

// Every intermediate state an interrupted reconcile can leave behind is
// driven to where it belongs, with or without status writes failing on the
// way, and with the replicas the workload had.
func TestLifecycle_InterruptedTransitionsComplete(t *testing.T) {
	pausedAt := metav1.NewTime(fixedTime.Add(-time.Hour))
	recorded := func() *v1alpha1.PauseStatus {
		return &v1alpha1.PauseStatus{PreviousReplicas: 3, PausedAt: pausedAt.DeepCopy()}
	}
	idlePolicy := &v1alpha1.IdlePolicySpec{IdleAfter: &metav1.Duration{Duration: time.Hour}}
	tests := []struct {
		name         string
		phase        v1alpha1.WorkloadPhase
		pause        *v1alpha1.PauseStatus
		requested    bool
		woken        bool
		dryRun       bool
		idlePolicy   *v1alpha1.IdlePolicySpec
		replicas     int32
		wantPhase    v1alpha1.WorkloadPhase
		wantReplicas int32
	}{
		{name: "pausing, to pause", phase: v1alpha1.PhasePausing, pause: recorded(), wantPhase: v1alpha1.PhasePaused},
		{name: "pausing on idle, to pause", phase: v1alpha1.PhasePausing, pause: recorded(), idlePolicy: idlePolicy,
			wantPhase: v1alpha1.PhasePaused},
		{name: "pausing, record from an earlier version missing", phase: v1alpha1.PhasePausing, replicas: 3,
			wantPhase: v1alpha1.PhasePaused},
		{name: "pausing, but now in dry-run", phase: v1alpha1.PhasePausing, pause: recorded(), dryRun: true,
			idlePolicy: idlePolicy, wantPhase: v1alpha1.PhaseRunning, wantReplicas: 3},
		{name: "paused, then dry-run switched on", phase: v1alpha1.PhasePaused, pause: recorded(), dryRun: true,
			idlePolicy: idlePolicy, wantPhase: v1alpha1.PhaseRunning, wantReplicas: 3},
		{name: "paused, woken", phase: v1alpha1.PhasePaused, pause: recorded(), woken: true,
			wantPhase: v1alpha1.PhaseRunning, wantReplicas: 3},
		{name: "paused, record from an earlier version missing, woken", phase: v1alpha1.PhasePaused, woken: true,
			wantPhase: v1alpha1.PhaseRunning, wantReplicas: 1},
		{name: "resuming", phase: v1alpha1.PhaseResuming, pause: recorded(), wantPhase: v1alpha1.PhaseRunning, wantReplicas: 3},
		{name: "resumed, but Running wasn't recorded", phase: v1alpha1.PhaseResuming, replicas: 3,
			wantPhase: v1alpha1.PhaseRunning, wantReplicas: 3},
		{name: "idle, active again", phase: v1alpha1.PhaseIdle, replicas: 3, idlePolicy: idlePolicy,
			wantPhase: v1alpha1.PhaseRunning, wantReplicas: 3},
		{name: "idle, pause requested", phase: v1alpha1.PhaseIdle, replicas: 3, requested: true,
			wantPhase: v1alpha1.PhasePaused},
		{name: "idle, idle policy removed", phase: v1alpha1.PhaseIdle, replicas: 3,
			wantPhase: v1alpha1.PhaseRunning, wantReplicas: 3},
		{name: "running, pause requested in dry-run", phase: v1alpha1.PhaseRunning, replicas: 3, dryRun: true,
			requested: true, wantPhase: v1alpha1.PhaseIdle, wantReplicas: 3},
	}
	for _, tt := range tests {
		for _, flaky := range []bool{false, true} {
			name := tt.name
			if flaky {
				name += ", status writes failing"
			}
			t.Run(name, func(t *testing.T) {
				workload := lifecycleWorkload("api", tt.phase)
				if tt.requested {
					withPauseRequested(workload)
				}
				if tt.woken {
					wokenByActivity(workload)
				}
				workload.Spec.DryRun = tt.dryRun
				workload.Spec.IdlePolicy = tt.idlePolicy
				workload.Status.Pause = tt.pause
				workload.Status.Activity = &v1alpha1.ActivityStatus{LastActivityTime: metav1.NewTime(fixedTime),
					LastEvaluatedTime: ptr.To(metav1.NewTime(fixedTime))}
				funcs := interceptor.Funcs{}
				if flaky {
					writes := 0
					funcs.SubResourceUpdate = func(ctx context.Context, c client.Client, sub string, obj client.Object,
						opts ...client.SubResourceUpdateOption) error {
						if _, ok := obj.(*v1alpha1.ManagedWorkload); ok && sub == statusSubresource {
							writes++
							if writes%2 == 1 {
								return apierrors.NewConflict(schema.GroupResource{Resource: "managedworkloads"}, "api",
									errors.New("the object has been modified"))
							}
						}
						return c.SubResource(sub).Update(ctx, obj, opts...)
					}
				}
				r := lifecycleReconciler(t, workload, tt.replicas, funcs)

				reconcileUntilSettled(t, r)

				got := getWorkload(t, r, "api")
				assert.Equal(t, tt.wantPhase, got.Status.Phase)
				assert.Equal(t, tt.wantReplicas, targetReplicas(t, r))
				if tt.wantPhase == v1alpha1.PhasePaused {
					require.NotNil(t, got.Status.Pause)
					assert.Equal(t, int32(3), got.Status.Pause.PreviousReplicas, "what it resumes to")
				} else {
					assert.Nil(t, got.Status.Pause)
				}
			})
		}
	}
}

// The hybernate.io/ignore label stops Hybernate managing a target. One it
// had paused is restored first, so the label never leaves it at zero.
func TestReconcile_IgnoreLabelRestoresAPausedTarget(t *testing.T) {
	workload := pausedWorkload(v1alpha1.PhasePaused)
	r := realPauserReconciler(t, workload)
	var target appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "api", Namespace: "default"}, &target))
	target.Labels = map[string]string{v1alpha1.LabelIgnore: v1alpha1.True}
	require.NoError(t, r.Update(context.Background(), &target))

	for range 2 {
		_, err := r.Reconcile(context.Background(), reconcileFor("api"))
		require.NoError(t, err)
	}

	got := getWorkload(t, r, "api")
	assert.Equal(t, int32(3), targetReplicas(t, r))
	assert.Equal(t, v1alpha1.PhaseRunning, got.Status.Phase)
	assert.Nil(t, got.Status.Pause)
	c := meta.FindStatusCondition(got.Status.Conditions, conditionTargetAvailable)
	require.NotNil(t, c)
	assert.Equal(t, "TargetIgnored", c.Reason)
}

// Repeating a reconcile that changes nothing must not repeat its events or
// rewrite status: an event for every check buries the ones that matter.
func TestReconcile_ReportsOnlyChanges(t *testing.T) {
	t.Run("target not found", func(t *testing.T) {
		workload := lifecycleWorkload("api", v1alpha1.PhaseRunning)
		r := newTestReconcilerWithTarget(t, workload, &stubPauser{}, false)
		_, err := r.Reconcile(context.Background(), reconcileFor("api"))
		require.NoError(t, err)
		version := getWorkload(t, r, "api").ResourceVersion
		for range 2 {
			_, err := r.Reconcile(context.Background(), reconcileFor("api"))
			require.NoError(t, err)
		}
		assert.Equal(t, 1, strings.Count(drainEvents(t, r), ReasonTargetNotFound))
		assert.Equal(t, version, getWorkload(t, r, "api").ResourceVersion, "nothing changed, so nothing is written")
	})
}

// A retry after a failed write counts an idle detection once, as it happened.
func TestReconcile_IdleDetectionIsCountedOnce(t *testing.T) {
	workload := lifecycleWorkload("counted-once", v1alpha1.PhaseRunning)
	workload.Spec.DryRun = true
	workload.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{IdleAfter: &metav1.Duration{Duration: time.Hour}}
	workload.Status.Activity = &v1alpha1.ActivityStatus{LastActivityTime: metav1.NewTime(fixedTime.Add(-2 * time.Hour)),
		LastEvaluatedTime: ptr.To(metav1.NewTime(fixedTime.Add(-30 * time.Second)))}
	r := lifecycleReconciler(t, workload, 1, failStatusWrite(inPhase(v1alpha1.PhaseIdle)))
	counted := metrics.IdleDetections.WithLabelValues("default", "counted-once")
	before := testutil.ToFloat64(counted)

	res, err := r.Reconcile(context.Background(), reconcileFor("counted-once"))
	require.NoError(t, err, "a conflict is retried, not reported")
	require.Equal(t, staleRetry, res.RequeueAfter)
	_, err = r.Reconcile(context.Background(), reconcileFor("counted-once"))
	require.NoError(t, err)

	require.Equal(t, v1alpha1.PhaseIdle, getWorkload(t, r, "counted-once").Status.Phase)
	assert.Equal(t, before+1, testutil.ToFloat64(counted))
	assert.Equal(t, 1, strings.Count(drainEvents(t, r), ReasonIdleDetected))
}

// No call a reconcile makes can wait forever: a hung API server, metrics
// API or Prometheus would otherwise hold a worker and everything queued
// behind it.
func TestReconcile_HasADeadline(t *testing.T) {
	var withoutDeadline []string
	funcs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := ctx.Deadline(); !ok {
				withoutDeadline = append(withoutDeadline, key.String())
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	workload := withPauseRequested(lifecycleWorkload("api", v1alpha1.PhaseRunning))
	r := lifecycleReconciler(t, workload, 3, funcs)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))

	require.NoError(t, err)
	assert.Empty(t, withoutDeadline)
}

// Different workloads are reconciled in parallel, sharing the reconciler's
// in-memory state. Run with -race.
func TestReconcile_WorkloadsInParallel(t *testing.T) {
	const n = 8
	objs := make([]client.Object, 0, 2*n)
	for i := range n {
		name := fmt.Sprintf("app-%d", i)
		w := lifecycleWorkload(name, v1alpha1.PhaseRunning)
		// The fake client sets no UIDs, and state is kept by UID, so without
		// one every workload would share one forecast, as none can for real.
		w.UID = types.UID(name)
		if i%2 == 0 {
			withPauseRequested(w)
		} else {
			w.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{IdleAfter: &metav1.Duration{Duration: time.Hour}}
		}
		target := targetDeploymentWithReplicas(name, "default", 2)
		objs = append(objs, w, target)
	}
	first := objs[0].(*v1alpha1.ManagedWorkload)
	r := lifecycleReconciler(t, first, 2, interceptor.Funcs{}, objs[2:]...)
	r.Recorder = events.NewFakeRecorder(10000)
	r.engines = newEngineRegistry(func() forecaster { return &stubForecaster{} })

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			for range 5 {
				_, _ = r.Reconcile(context.Background(), reconcileFor(fmt.Sprintf("app-%d", i))) // only the end state matters
			}
		})
	}
	wg.Wait()

	for i := range n {
		name := fmt.Sprintf("app-%d", i)
		want := v1alpha1.PhaseRunning
		if i%2 == 0 {
			want = v1alpha1.PhasePaused
		}
		assert.Equal(t, want, getWorkload(t, r, name).Status.Phase, name)
	}
}
