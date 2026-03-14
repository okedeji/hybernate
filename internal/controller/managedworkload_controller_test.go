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
	pauseDone  bool
	pauseErr   error
	resumeDone bool
	resumeErr  error
	pauseCalls int
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
	destroyDone    bool
	destroyErr     error
	cleanupDone    bool
	cleanupErr     error
	destroyCalls   int
	cleanupCalls   int
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
	return s
}

func newTestReconciler(t *testing.T, workload *v1alpha1.ManagedWorkload, pauser *stubPauser, destroyer *stubDestroyer) *Reconciler {
	t.Helper()
	scheme := testScheme(t)

	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.ManagedWorkload{})
	if workload != nil {
		builder = builder.WithObjects(workload)
	}

	return &Reconciler{
		Client:    builder.Build(),
		Scheme:    scheme,
		Recorder:  record.NewFakeRecorder(10),
		pauser:    pauser,
		destroyer: destroyer,
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

func getWorkload(t *testing.T, r *Reconciler, name string) *v1alpha1.ManagedWorkload {
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
			Target: v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
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
			Target:       v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
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
			Target:       v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
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
			Target:       v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
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
			Target:       v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
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
			Target:       v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
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
			Target:       v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
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
			Target:       v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
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
			Target: v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
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
			Target: v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
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
			Target: v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
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
			Target: v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
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
			Target: v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
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
			Target: v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
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
			Target: v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
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

// --- No desiredState ---

func TestReconcile_NoDesiredStateIsNoOp(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning},
	}

	pauser := &stubPauser{}
	destroyer := &stubDestroyer{}
	r := newTestReconciler(t, workload, pauser, destroyer)

	result, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
	assert.Equal(t, 0, pauser.pauseCalls)
	assert.Equal(t, 0, pauser.resumeCalls)
	assert.Equal(t, 0, destroyer.destroyCalls)
}
