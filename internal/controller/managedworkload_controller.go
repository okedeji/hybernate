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
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/lifecycle"
)

const finalizerName = "hybernate.io/cleanup"

// Reconciler drives ManagedWorkload objects through their lifecycle.
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	pauser    lifecyclePauser
	destroyer lifecycleDestroyer
	clock     func() time.Time
}

type lifecyclePauser interface {
	Pause(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error)
	Resume(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error)
}

type lifecycleDestroyer interface {
	Destroy(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error)
	CleanupPVCs(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error)
}

// +kubebuilder:rbac:groups=hybernate.io,resources=managedworkloads,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hybernate.io,resources=managedworkloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hybernate.io,resources=managedworkloads/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;update;delete
// +kubebuilder:rbac:groups=apps,resources=deployments/scale;statefulsets/scale,verbs=get;update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile evaluates the current state of a ManagedWorkload and acts on it.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var workload v1alpha1.ManagedWorkload
	if err := r.Get(ctx, req.NamespacedName, &workload); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion with finalizer.
	if !workload.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &workload)
	}

	if err := r.ensureFinalizer(ctx, &workload); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring finalizer: %w", err)
	}

	// Set initial phase.
	if workload.Status.Phase == "" {
		return r.transition(ctx, &workload, v1alpha1.PhaseRunning, "Created")
	}

	// --- Pass 1: Manual lifecycle ---

	result, err := r.reconcileDesiredState(ctx, &workload)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result != nil {
		return *result, nil
	}

	// --- Housekeeping ---

	result, err = r.reconcileHousekeeping(ctx, &workload)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result != nil {
		return *result, nil
	}

	log.Info("reconciled", "phase", workload.Status.Phase)
	return ctrl.Result{}, nil
}

func (r *Reconciler) reconcileDesiredState(ctx context.Context, workload *v1alpha1.ManagedWorkload) (*ctrl.Result, error) {
	if workload.Spec.DesiredState == nil {
		return nil, nil
	}

	switch *workload.Spec.DesiredState {
	case v1alpha1.DesiredStatePaused:
		return r.handlePause(ctx, workload)
	case v1alpha1.DesiredStateRunning:
		return r.handleResume(ctx, workload)
	case v1alpha1.DesiredStateDestroyed:
		return r.handleDestroy(ctx, workload)
	default:
		return nil, nil
	}
}

func (r *Reconciler) handlePause(ctx context.Context, workload *v1alpha1.ManagedWorkload) (*ctrl.Result, error) {
	phase := workload.Status.Phase
	if phase == v1alpha1.PhasePaused {
		return nil, nil
	}
	if phase != v1alpha1.PhaseRunning && phase != v1alpha1.PhaseIdle && phase != v1alpha1.PhasePausing {
		return nil, nil
	}

	if phase != v1alpha1.PhasePausing {
		if _, err := r.transition(ctx, workload, v1alpha1.PhasePausing, "PauseRequested"); err != nil {
			return nil, err
		}
	}

	done, err := r.pauser.Pause(ctx, workload)
	if err != nil {
		return nil, fmt.Errorf("pausing workload: %w", err)
	}
	if !done {
		result := ctrl.Result{RequeueAfter: 5 * time.Second}
		return &result, nil
	}

	result, err := r.transition(ctx, workload, v1alpha1.PhasePaused, "Paused")
	if err != nil {
		return nil, err
	}
	r.Recorder.Eventf(workload, "Normal", "Paused", "Workload %s paused", workload.Spec.Target.Name)
	return &result, nil
}

func (r *Reconciler) handleResume(ctx context.Context, workload *v1alpha1.ManagedWorkload) (*ctrl.Result, error) {
	phase := workload.Status.Phase
	if phase == v1alpha1.PhaseRunning {
		return nil, nil
	}
	if phase != v1alpha1.PhasePaused && phase != v1alpha1.PhaseResuming {
		return nil, nil
	}

	if phase != v1alpha1.PhaseResuming {
		if _, err := r.transition(ctx, workload, v1alpha1.PhaseResuming, "ResumeRequested"); err != nil {
			return nil, err
		}
	}

	done, err := r.pauser.Resume(ctx, workload)
	if err != nil {
		return nil, fmt.Errorf("resuming workload: %w", err)
	}
	if !done {
		result := ctrl.Result{RequeueAfter: 5 * time.Second}
		return &result, nil
	}

	result, err := r.transition(ctx, workload, v1alpha1.PhaseRunning, "Resumed")
	if err != nil {
		return nil, err
	}
	r.Recorder.Eventf(workload, "Normal", "Resumed", "Workload %s resumed", workload.Spec.Target.Name)
	return &result, nil
}

func (r *Reconciler) handleDestroy(ctx context.Context, workload *v1alpha1.ManagedWorkload) (*ctrl.Result, error) {
	phase := workload.Status.Phase
	if phase == v1alpha1.PhaseDestroyed {
		return nil, nil
	}
	if phase == v1alpha1.PhaseDestroying {
		return nil, nil
	}

	if _, err := r.transition(ctx, workload, v1alpha1.PhaseDestroying, "DestroyRequested"); err != nil {
		return nil, err
	}

	done, err := r.destroyer.Destroy(ctx, workload)
	if err != nil {
		return nil, fmt.Errorf("destroying workload: %w", err)
	}
	if !done {
		result := ctrl.Result{RequeueAfter: 5 * time.Second}
		return &result, nil
	}

	result, err := r.transition(ctx, workload, v1alpha1.PhaseDestroyed, "Destroyed")
	if err != nil {
		return nil, err
	}
	r.Recorder.Eventf(workload, "Normal", "Destroyed", "Workload %s destroyed", workload.Spec.Target.Name)
	return &result, nil
}

// --- Housekeeping ---

func (r *Reconciler) reconcileHousekeeping(ctx context.Context, workload *v1alpha1.ManagedWorkload) (*ctrl.Result, error) {
	if result, err := r.checkPauseExpiry(ctx, workload); result != nil || err != nil {
		return result, err
	}

	if result, err := r.checkPVCRetention(ctx, workload); result != nil || err != nil {
		return result, err
	}

	if result, err := r.checkPVCRetentionWarning(ctx, workload); result != nil || err != nil {
		return result, err
	}

	return nil, nil
}

func (r *Reconciler) checkPauseExpiry(ctx context.Context, workload *v1alpha1.ManagedWorkload) (*ctrl.Result, error) {
	if workload.Status.Phase != v1alpha1.PhasePaused {
		return nil, nil
	}
	if workload.Spec.Pause == nil || workload.Spec.Pause.ExpireAfter == nil {
		return nil, nil
	}
	if workload.Status.Pause == nil || workload.Status.Pause.PausedAt == nil {
		return nil, nil
	}

	expiry := workload.Status.Pause.PausedAt.Add(workload.Spec.Pause.ExpireAfter.Duration)
	now := r.now()

	if now.Before(expiry) {
		remaining := expiry.Sub(now)
		result := ctrl.Result{RequeueAfter: remaining}
		return &result, nil
	}

	r.Recorder.Eventf(workload, "Normal", "PauseExpired",
		"Pause expired after %s, executing %s", workload.Spec.Pause.ExpireAfter.Duration, workload.Spec.Pause.ExpireAction)

	switch workload.Spec.Pause.ExpireAction {
	case v1alpha1.ExpireActionResume:
		return r.handleResume(ctx, workload)
	default:
		return r.handleDestroy(ctx, workload)
	}
}

func (r *Reconciler) checkPVCRetention(ctx context.Context, workload *v1alpha1.ManagedWorkload) (*ctrl.Result, error) {
	if workload.Status.Phase != v1alpha1.PhaseDestroyed {
		return nil, nil
	}
	if workload.Status.Destroy == nil || workload.Status.Destroy.PVCRetentionExpiresAt == nil {
		return nil, nil
	}

	now := r.now()
	expiry := workload.Status.Destroy.PVCRetentionExpiresAt.Time
	if now.Before(expiry) {
		remaining := expiry.Sub(now)
		result := ctrl.Result{RequeueAfter: remaining}
		return &result, nil
	}

	done, err := r.destroyer.CleanupPVCs(ctx, workload)
	if err != nil {
		return nil, fmt.Errorf("cleaning up pvcs: %w", err)
	}
	if !done {
		result := ctrl.Result{RequeueAfter: 30 * time.Second}
		return &result, nil
	}

	if err := r.Status().Update(ctx, workload); err != nil {
		return nil, fmt.Errorf("updating status after pvc cleanup: %w", err)
	}

	r.Recorder.Eventf(workload, "Normal", "PVCsCleaned", "PVCs for %s cleaned up after retention period", workload.Spec.Target.Name)
	result := ctrl.Result{}
	return &result, nil
}

func (r *Reconciler) checkPVCRetentionWarning(_ context.Context, workload *v1alpha1.ManagedWorkload) (*ctrl.Result, error) {
	if workload.Status.Phase != v1alpha1.PhaseDestroyed {
		return nil, nil
	}
	if workload.Spec.Destroy == nil || workload.Spec.Destroy.PVCRetentionWarning == nil {
		return nil, nil
	}
	if workload.Status.Destroy == nil || workload.Status.Destroy.PVCRetentionExpiresAt == nil {
		return nil, nil
	}

	expiry := workload.Status.Destroy.PVCRetentionExpiresAt.Time
	warningWindow := workload.Spec.Destroy.PVCRetentionWarning.Duration
	warningTime := expiry.Add(-warningWindow)
	now := r.now()

	if now.Before(warningTime) {
		return nil, nil
	}

	remaining := expiry.Sub(now).Round(time.Minute)
	r.Recorder.Eventf(workload, "Warning", "PVCRetentionExpiring",
		"PVCs for %s will be deleted in %s", workload.Spec.Target.Name, remaining)

	return nil, nil
}

// --- Finalizer ---

func (r *Reconciler) ensureFinalizer(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	if controllerutil.ContainsFinalizer(workload, finalizerName) {
		return nil
	}
	controllerutil.AddFinalizer(workload, finalizerName)
	return r.Update(ctx, workload)
}

func (r *Reconciler) reconcileDelete(ctx context.Context, workload *v1alpha1.ManagedWorkload) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(workload, finalizerName) {
		return ctrl.Result{}, nil
	}

	// If destroyed with PVC retention pending, clean up.
	if workload.Status.Destroy != nil && workload.Status.Destroy.PVCRetentionExpiresAt != nil {
		now := r.now()
		expiry := workload.Status.Destroy.PVCRetentionExpiresAt.Time
		if now.Before(expiry) {
			remaining := expiry.Sub(now)
			return ctrl.Result{RequeueAfter: remaining}, nil
		}

		done, err := r.destroyer.CleanupPVCs(ctx, workload)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("cleaning up pvcs during deletion: %w", err)
		}
		if !done {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	}

	controllerutil.RemoveFinalizer(workload, finalizerName)
	if err := r.Update(ctx, workload); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// --- Helpers ---

func (r *Reconciler) transition(ctx context.Context, workload *v1alpha1.ManagedWorkload, phase v1alpha1.WorkloadPhase, reason string) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	old := workload.Status.Phase
	workload.Status.Phase = phase

	now := r.clockTime()
	workload.Status.LastTransitionTime = &now

	if err := r.Status().Update(ctx, workload); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating phase to %s: %w", phase, err)
	}
	log.Info("phase transition", "from", old, "to", phase, "reason", reason)
	return ctrl.Result{}, nil
}

func (r *Reconciler) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

func (r *Reconciler) clockTime() metav1.Time {
	return metav1.NewTime(r.now())
}

func (r *Reconciler) initDefaults() {
	if r.pauser == nil {
		r.pauser = lifecycle.NewPauser(r.Client)
	}
	if r.destroyer == nil {
		r.destroyer = lifecycle.NewDestroyer(r.Client)
	}
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.initDefaults()
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ManagedWorkload{}).
		Named("managedworkload").
		Complete(r)
}
