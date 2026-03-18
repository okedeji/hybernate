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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

var fixedTime = time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)

type stubPauser struct {
	pauseDone   bool
	pauseErr    error
	resumeDone  bool
	resumeErr   error
	pauseCalls  int
	resumeCalls int
}

func (s *stubPauser) Pause(_ context.Context, _ *v1alpha1.ManagedWorkload) (bool, error) {
	s.pauseCalls++
	return s.pauseDone, s.pauseErr
}

func (s *stubPauser) Resume(_ context.Context, _ *v1alpha1.ManagedWorkload) (bool, error) {
	s.resumeCalls++
	return s.resumeDone, s.resumeErr
}

type stubDestroyer struct {
	destroyDone  bool
	destroyErr   error
	cleanupDone  bool
	cleanupErr   error
	destroyCalls int
	cleanupCalls int
}

func (s *stubDestroyer) Destroy(_ context.Context, _ *v1alpha1.ManagedWorkload) (bool, error) {
	s.destroyCalls++
	return s.destroyDone, s.destroyErr
}

func (s *stubDestroyer) CleanupPVCs(_ context.Context, _ *v1alpha1.ManagedWorkload) (bool, error) {
	s.cleanupCalls++
	return s.cleanupDone, s.cleanupErr
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
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

func newTestReconcilerWithReplicas(t *testing.T, workload *v1alpha1.ManagedWorkload, pauser *stubPauser, destroyer *stubDestroyer, replicas int32) *Reconciler {
	t.Helper()
	scheme := testScheme(t)

	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.ManagedWorkload{})
	if workload != nil {
		builder = builder.WithObjects(workload)
		builder = builder.WithObjects(targetDeploymentWithReplicas(workload.Spec.Target.Name, workload.Namespace, replicas))
	}

	return &Reconciler{
		Client:    builder.Build(),
		Scheme:    scheme,
		Recorder:  record.NewFakeRecorder(10),
		pauser:    pauser,
		destroyer: destroyer,
		engines:   newEngineRegistry(func(_ int) forecaster { return &stubForecaster{} }),
		clock:     func() time.Time { return fixedTime },
	}
}

func newTestReconciler(t *testing.T, workload *v1alpha1.ManagedWorkload, pauser *stubPauser, destroyer *stubDestroyer) *Reconciler {
	t.Helper()
	return newTestReconcilerWithTarget(t, workload, pauser, destroyer, true)
}

func newTestReconcilerWithTarget(t *testing.T, workload *v1alpha1.ManagedWorkload, pauser *stubPauser, destroyer *stubDestroyer, createTarget bool) *Reconciler {
	t.Helper()
	scheme := testScheme(t)

	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.ManagedWorkload{})
	if workload != nil {
		builder = builder.WithObjects(workload)
		if createTarget {
			builder = builder.WithObjects(targetDeployment(workload.Spec.Target.Name, workload.Namespace))
		}
	}

	return &Reconciler{
		Client:    builder.Build(),
		Scheme:    scheme,
		Recorder:  record.NewFakeRecorder(10),
		pauser:    pauser,
		destroyer: destroyer,
		engines:   newEngineRegistry(func(_ int) forecaster { return &stubForecaster{} }),
		clock:     func() time.Time { return fixedTime },
	}
}

func reconcileFor(name string) reconcile.Request {
	return reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
	}
}

func desiredState(s v1alpha1.DesiredState) *v1alpha1.DesiredState {
	return &s
}

func getWorkload(t *testing.T, r *Reconciler, name string) *v1alpha1.ManagedWorkload { //nolint:unparam
	t.Helper()
	var w v1alpha1.ManagedWorkload
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &w))
	return &w
}

// --- Initial reconcile ---

func TestReconcile_SetsInitialPhaseToRunning(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
	}

	r := newTestReconciler(t, workload, &stubPauser{}, &stubDestroyer{})
	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	w := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseRunning, w.Status.Phase)
	assert.True(t, len(w.Finalizers) > 0, "finalizer should be set")
}

func TestReconcile_NotFoundIsNoOp(t *testing.T) {
	r := newTestReconciler(t, nil, &stubPauser{}, &stubDestroyer{})
	result, err := r.Reconcile(context.Background(), reconcileFor("missing"))
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

// --- Pause ---

func TestReconcile_PauseTransitions(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:       v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			DesiredState: desiredState(v1alpha1.DesiredStatePaused),
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning},
	}

	pauser := &stubPauser{pauseDone: true}
	r := newTestReconciler(t, workload, pauser, &stubDestroyer{})

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	w := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhasePaused, w.Status.Phase)
	assert.Equal(t, 1, pauser.pauseCalls)
}

func TestReconcile_PauseNotDoneRequeues(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:       v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			DesiredState: desiredState(v1alpha1.DesiredStatePaused),
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning},
	}

	pauser := &stubPauser{pauseDone: false}
	r := newTestReconciler(t, workload, pauser, &stubDestroyer{})

	result, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, result.RequeueAfter)
}

func TestReconcile_AlreadyPausedIsNoOp(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:       v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			DesiredState: desiredState(v1alpha1.DesiredStatePaused),
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhasePaused},
	}

	pauser := &stubPauser{}
	r := newTestReconciler(t, workload, pauser, &stubDestroyer{})

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	assert.Equal(t, 0, pauser.pauseCalls)
}

// --- Resume ---

func TestReconcile_ResumeTransitions(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:       v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			DesiredState: desiredState(v1alpha1.DesiredStateRunning),
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhasePaused},
	}

	pauser := &stubPauser{resumeDone: true}
	r := newTestReconciler(t, workload, pauser, &stubDestroyer{})

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	w := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseRunning, w.Status.Phase)
	assert.Equal(t, 1, pauser.resumeCalls)
}

func TestReconcile_ResumeNotReadyRequeues(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:       v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			DesiredState: desiredState(v1alpha1.DesiredStateRunning),
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhasePaused},
	}

	pauser := &stubPauser{resumeDone: false}
	r := newTestReconciler(t, workload, pauser, &stubDestroyer{})

	result, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, result.RequeueAfter)
}

// --- Destroy ---

func TestReconcile_DestroyTransitions(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:       v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			DesiredState: desiredState(v1alpha1.DesiredStateDestroyed),
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning},
	}

	destroyer := &stubDestroyer{destroyDone: true}
	r := newTestReconciler(t, workload, &stubPauser{}, destroyer)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	w := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseDestroyed, w.Status.Phase)
	assert.Equal(t, 1, destroyer.destroyCalls)
}

func TestReconcile_AlreadyDestroyedIsNoOp(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:       v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			DesiredState: desiredState(v1alpha1.DesiredStateDestroyed),
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseDestroyed},
	}

	destroyer := &stubDestroyer{}
	r := newTestReconciler(t, workload, &stubPauser{}, destroyer)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	assert.Equal(t, 0, destroyer.destroyCalls)
}

// --- Pause Expiry ---

func TestReconcile_PauseExpiryDestroysWorkload(t *testing.T) {
	pausedAt := metav1.NewTime(fixedTime.Add(-2 * time.Hour))
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			Pause: &v1alpha1.PauseSpec{
				ExpireAfter:  &metav1.Duration{Duration: 1 * time.Hour},
				ExpireAction: v1alpha1.ExpireActionDestroy,
			},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase: v1alpha1.PhasePaused,
			Pause: &v1alpha1.PauseStatus{
				PreviousReplicas: 3,
				PausedAt:         &pausedAt,
			},
		},
	}

	destroyer := &stubDestroyer{destroyDone: true}
	r := newTestReconciler(t, workload, &stubPauser{}, destroyer)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	w := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseDestroyed, w.Status.Phase)
	assert.Equal(t, 1, destroyer.destroyCalls)
}

func TestReconcile_PauseExpiryResumesWorkload(t *testing.T) {
	pausedAt := metav1.NewTime(fixedTime.Add(-2 * time.Hour))
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			Pause: &v1alpha1.PauseSpec{
				ExpireAfter:  &metav1.Duration{Duration: 1 * time.Hour},
				ExpireAction: v1alpha1.ExpireActionResume,
			},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase: v1alpha1.PhasePaused,
			Pause: &v1alpha1.PauseStatus{
				PreviousReplicas: 3,
				PausedAt:         &pausedAt,
			},
		},
	}

	pauser := &stubPauser{resumeDone: true}
	r := newTestReconciler(t, workload, pauser, &stubDestroyer{})

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	w := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseRunning, w.Status.Phase)
	assert.Equal(t, 1, pauser.resumeCalls)
}

func TestReconcile_PauseNotExpiredRequeuesWithRemaining(t *testing.T) {
	pausedAt := metav1.NewTime(fixedTime.Add(-30 * time.Minute))
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			Pause: &v1alpha1.PauseSpec{
				ExpireAfter: &metav1.Duration{Duration: 1 * time.Hour},
			},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase: v1alpha1.PhasePaused,
			Pause: &v1alpha1.PauseStatus{
				PreviousReplicas: 3,
				PausedAt:         &pausedAt,
			},
		},
	}

	r := newTestReconciler(t, workload, &stubPauser{}, &stubDestroyer{})

	result, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	assert.Equal(t, 30*time.Minute, result.RequeueAfter)
}

// --- PVC Retention ---

func TestReconcile_PVCRetentionCleansUpAfterExpiry(t *testing.T) {
	destroyedAt := metav1.NewTime(fixedTime.Add(-8 * time.Hour))
	expiresAt := metav1.NewTime(fixedTime.Add(-1 * time.Hour))
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase: v1alpha1.PhaseDestroyed,
			Destroy: &v1alpha1.DestroyStatus{
				DestroyedAt:           &destroyedAt,
				PVCRetentionExpiresAt: &expiresAt,
			},
		},
	}

	destroyer := &stubDestroyer{cleanupDone: true}
	r := newTestReconciler(t, workload, &stubPauser{}, destroyer)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	assert.Equal(t, 1, destroyer.cleanupCalls)
}

func TestReconcile_PVCRetentionWaitsBeforeExpiry(t *testing.T) {
	destroyedAt := metav1.NewTime(fixedTime.Add(-1 * time.Hour))
	expiresAt := metav1.NewTime(fixedTime.Add(6 * time.Hour))
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase: v1alpha1.PhaseDestroyed,
			Destroy: &v1alpha1.DestroyStatus{
				DestroyedAt:           &destroyedAt,
				PVCRetentionExpiresAt: &expiresAt,
			},
		},
	}

	destroyer := &stubDestroyer{}
	r := newTestReconciler(t, workload, &stubPauser{}, destroyer)

	result, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	assert.Equal(t, 6*time.Hour, result.RequeueAfter)
	assert.Equal(t, 0, destroyer.cleanupCalls)
}

// --- Finalizer + Deletion ---

func TestReconcile_FinalizerAddedOnFirstReconcile(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
	}

	r := newTestReconciler(t, workload, &stubPauser{}, &stubDestroyer{})
	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	w := getWorkload(t, r, "api")
	assert.Contains(t, w.Finalizers, finalizerName)
}

func TestReconcile_DeletionRemovesFinalizerWhenNoPVCRetention(t *testing.T) {
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

	r := newTestReconciler(t, workload, &stubPauser{}, &stubDestroyer{})
	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	// Object is fully deleted once finalizer is removed by the fake client.
	var w v1alpha1.ManagedWorkload
	err = r.Get(context.Background(), types.NamespacedName{Name: "api", Namespace: "default"}, &w)
	assert.True(t, err != nil, "object should be deleted after finalizer removal")
}

// --- Target check ---

func TestReconcile_TargetNotFoundSetsConditionAndRequeues(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning},
	}

	r := newTestReconcilerWithTarget(t, workload, &stubPauser{}, &stubDestroyer{}, false)

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

// --- Drift detection ---

func TestReconcile_DriftWarnEmitsEventOnly(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:         v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			ConflictAction: v1alpha1.ConflictActionWarn,
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase: v1alpha1.PhaseRunning,
			Scale: &v1alpha1.ScaleStatus{CurrentReplicas: 3},
		},
	}

	r := newTestReconcilerWithReplicas(t, workload, &stubPauser{}, &stubDestroyer{}, 5)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	// Replicas should NOT be changed — warn only emits an event.
	var dep appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "api", Namespace: "default"}, &dep))
	assert.Equal(t, int32(5), *dep.Spec.Replicas)
}

func TestReconcile_DriftEnforceCorrects(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:         v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			ConflictAction: v1alpha1.ConflictActionEnforce,
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase: v1alpha1.PhaseRunning,
			Scale: &v1alpha1.ScaleStatus{CurrentReplicas: 3},
		},
	}

	r := newTestReconcilerWithReplicas(t, workload, &stubPauser{}, &stubDestroyer{}, 5)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	var dep appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "api", Namespace: "default"}, &dep))
	assert.Equal(t, int32(3), *dep.Spec.Replicas)

	w := getWorkload(t, r, "api")
	assert.NotNil(t, w.Status.LastActedAt)
}

func TestReconcile_DriftDeferAcceptsExternalChange(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:         v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			ConflictAction: v1alpha1.ConflictActionDefer,
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase: v1alpha1.PhaseRunning,
			Scale: &v1alpha1.ScaleStatus{CurrentReplicas: 3},
		},
	}

	r := newTestReconcilerWithReplicas(t, workload, &stubPauser{}, &stubDestroyer{}, 5)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	// Replicas untouched on the target.
	var dep appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "api", Namespace: "default"}, &dep))
	assert.Equal(t, int32(5), *dep.Spec.Replicas)

	// Status updated to match actual.
	w := getWorkload(t, r, "api")
	assert.Equal(t, int32(5), w.Status.Scale.CurrentReplicas)
}

func TestReconcile_NoDriftWhenReplicasMatch(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:         v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			ConflictAction: v1alpha1.ConflictActionEnforce,
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase: v1alpha1.PhaseRunning,
			Scale: &v1alpha1.ScaleStatus{CurrentReplicas: 3},
		},
	}

	r := newTestReconcilerWithReplicas(t, workload, &stubPauser{}, &stubDestroyer{}, 3)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	// No LastActedAt stamped since no drift correction happened.
	w := getWorkload(t, r, "api")
	assert.Nil(t, w.Status.LastActedAt)
}

func TestReconcile_NoDriftWithoutBaseline(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:         v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			ConflictAction: v1alpha1.ConflictActionEnforce,
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase: v1alpha1.PhaseRunning,
			// No Scale status — operator never scaled this workload.
		},
	}

	r := newTestReconcilerWithReplicas(t, workload, &stubPauser{}, &stubDestroyer{}, 5)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	// No drift action — no baseline to compare against.
	var dep appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "api", Namespace: "default"}, &dep))
	assert.Equal(t, int32(5), *dep.Spec.Replicas)
}

// --- No desiredState ---

func TestReconcile_NoDesiredStateIsNoOp(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning},
	}

	pauser := &stubPauser{}
	destroyer := &stubDestroyer{}
	r := newTestReconciler(t, workload, pauser, destroyer)

	result, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	// Automation requeues for prediction learning (Observing phase).
	assert.Equal(t, 1*time.Hour, result.RequeueAfter)
	assert.Equal(t, 0, pauser.pauseCalls)
	assert.Equal(t, 0, pauser.resumeCalls)
	assert.Equal(t, 0, destroyer.destroyCalls)
}

// --- Duplicate target detection ---

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
	recorder := record.NewFakeRecorder(10)
	k := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.ManagedWorkload{}).
		WithObjects(older, newer, targetDeployment("idle-app", "default")).
		Build()

	r := &Reconciler{
		Client:    k,
		Scheme:    scheme,
		Recorder:  recorder,
		pauser:    &stubPauser{},
		destroyer: &stubDestroyer{},
		engines:   newEngineRegistry(func(_ int) forecaster { return &stubForecaster{} }),
		clock:     func() time.Time { return fixedTime },
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
		Client:    k,
		Scheme:    scheme,
		Recorder:  record.NewFakeRecorder(10),
		pauser:    &stubPauser{},
		destroyer: &stubDestroyer{},
		engines:   newEngineRegistry(func(_ int) forecaster { return &stubForecaster{} }),
		clock:     func() time.Time { return fixedTime },
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
		Client:    k,
		Scheme:    scheme,
		Recorder:  record.NewFakeRecorder(10),
		pauser:    &stubPauser{},
		destroyer: &stubDestroyer{},
		engines:   newEngineRegistry(func(_ int) forecaster { return &stubForecaster{} }),
		clock:     func() time.Time { return fixedTime },
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
