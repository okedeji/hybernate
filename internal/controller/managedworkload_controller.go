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

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/forecast"
	"github.com/okedeji/hybernate/internal/lifecycle"
	"github.com/okedeji/hybernate/internal/metrics"
)

const finalizerName = v1alpha1.FinalizerCleanup

// Reconciler drives ManagedWorkload objects through their lifecycle.
type Reconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Recorder      record.EventRecorder
	PrometheusURL string

	pauser          lifecyclePauser
	destroyer       lifecycleDestroyer
	lifecycleScaler lifecycleScaler
	idle            idleEvaluator
	scale           scaleEvaluator
	metrics         metricsReader
	engines         *engineRegistry
	prometheusURL   string
	clock           func() time.Time
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
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile evaluates the current state of a ManagedWorkload and acts on it.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)

	defer func() {
		if retErr != nil {
			metrics.ReconcileErrors.WithLabelValues("managedworkload").Inc()
		}
	}()

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

	// --- Target check + drift detection ---

	if workload.Status.Phase != v1alpha1.PhaseDestroyed && workload.Status.Phase != v1alpha1.PhaseDestroying {
		target, err := r.checkTarget(ctx, &workload)
		if err != nil {
			return ctrl.Result{}, err
		}
		if target == nil {
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
		}
		if result, err := r.checkDrift(ctx, &workload, target); result != nil || err != nil {
			if err != nil {
				return ctrl.Result{}, err
			}
			return *result, nil
		}
	}

	// --- Manual lifecycle ---

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

	// --- Automation ---

	result, err = r.reconcileAutomation(ctx, &workload)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result != nil {
		return *result, nil
	}

	// --- Cost tracking ---
	r.accumulateCost(ctx, &workload)

	logger.Info("reconciled", "phase", workload.Status.Phase)
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

	// Capture resource profile before scaling to zero for cost savings tracking.
	if workload.Status.Pause == nil || workload.Status.Pause.Resources == nil {
		snap := r.captureResourceSnapshot(ctx, workload)
		if workload.Status.Pause != nil {
			workload.Status.Pause.Resources = snap
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

	r.stampLastActed(workload)
	r.observeActionDuration(workload, "pause")
	result, err := r.transition(ctx, workload, v1alpha1.PhasePaused, "Paused")
	if err != nil {
		return nil, err
	}
	r.emitEvent(workload, false, "Normal", ReasonPaused, "paused")
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

	r.stampLastActed(workload)
	r.observeActionDuration(workload, "resume")
	result, err := r.transition(ctx, workload, v1alpha1.PhaseRunning, "Resumed")
	if err != nil {
		return nil, err
	}
	r.emitEvent(workload, false, "Normal", ReasonResumed, "resumed")
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

	// Capture resource profile before deletion for cost savings tracking.
	snap := r.captureResourceSnapshot(ctx, workload)

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

	r.stampLastActed(workload)
	r.observeActionDuration(workload, "destroy")
	if workload.Status.Destroy == nil {
		workload.Status.Destroy = &v1alpha1.DestroyStatus{}
	}
	workload.Status.Destroy.Resources = snap
	result, err := r.transition(ctx, workload, v1alpha1.PhaseDestroyed, "Destroyed")
	if err != nil {
		return nil, err
	}
	r.emitEvent(workload, false, "Normal", ReasonDestroyed, "destroyed")
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

	r.emitEvent(workload, false, "Normal", ReasonPauseExpired,
		"pause expired after %s, executing %s", workload.Spec.Pause.ExpireAfter.Duration, workload.Spec.Pause.ExpireAction)

	switch workload.Spec.Pause.ExpireAction {
	case v1alpha1.ExpireActionResume:
		metrics.PauseExpiryActions.WithLabelValues("resume").Inc()
		return r.handleResume(ctx, workload)
	default:
		metrics.PauseExpiryActions.WithLabelValues("destroy").Inc()
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
		metrics.PVCRetentionRemaining.WithLabelValues(workload.Namespace, workload.Spec.Target.Name).Set(remaining.Seconds())
		result := ctrl.Result{RequeueAfter: remaining}
		return &result, nil
	}

	metrics.PVCRetentionRemaining.WithLabelValues(workload.Namespace, workload.Spec.Target.Name).Set(0)
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

	r.emitEvent(workload, false, "Normal", ReasonPVCsCleaned, "PVCs cleaned up after retention period")
	result := ctrl.Result{}
	return &result, nil
}

func (r *Reconciler) checkPVCRetentionWarning(_ context.Context, workload *v1alpha1.ManagedWorkload) (*ctrl.Result, error) { //nolint:unparam
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
	r.emitEvent(workload, false, "Warning", ReasonPVCRetentionExpiring,
		"PVCs will be deleted in %s", remaining)

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

// --- Target ---

const conditionTargetAvailable = "TargetAvailable"

// checkTarget verifies the target workload exists. Returns the target object
// on success, nil when not found (condition set, status updated), or an error.
func (r *Reconciler) checkTarget(ctx context.Context, workload *v1alpha1.ManagedWorkload) (client.Object, error) {
	ref := workload.Spec.Target
	nn := types.NamespacedName{Name: ref.Name, Namespace: workload.Namespace}

	var obj client.Object
	switch ref.Kind {
	case v1alpha1.TargetKindDeployment:
		obj = &appsv1.Deployment{}
	case v1alpha1.TargetKindStatefulSet:
		obj = &appsv1.StatefulSet{}
	default:
		return nil, fmt.Errorf("unsupported target kind: %s", ref.Kind)
	}

	err := r.Get(ctx, nn, obj)
	if apierrors.IsNotFound(err) {
		r.setCondition(workload, conditionTargetAvailable, metav1.ConditionFalse, "TargetNotFound",
			fmt.Sprintf("%s %s not found", ref.Kind, ref.Name))
		if err := r.Status().Update(ctx, workload); err != nil {
			return nil, fmt.Errorf("updating target condition: %w", err)
		}
		r.emitEvent(workload, false, "Warning", "TargetNotFound",
			"%s %s not found", ref.Kind, ref.Name)
		metrics.TargetUnavailable.WithLabelValues(workload.Namespace, workload.Spec.Target.Name).Inc()
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checking target %s %s: %w", ref.Kind, ref.Name, err)
	}

	if obj.GetLabels()[v1alpha1.LabelIgnore] == "true" {
		r.setCondition(workload, conditionTargetAvailable, metav1.ConditionFalse, "TargetIgnored",
			fmt.Sprintf("%s %s has %s label", ref.Kind, ref.Name, v1alpha1.LabelIgnore))
		if err := r.Status().Update(ctx, workload); err != nil {
			return nil, fmt.Errorf("updating target condition: %w", err)
		}
		return nil, nil
	}

	r.setCondition(workload, conditionTargetAvailable, metav1.ConditionTrue, "TargetExists", "")
	return obj, nil
}

// checkDrift compares actual replicas on the target against what the operator
// last set. When they differ, the conflict policy decides the response.
func (r *Reconciler) checkDrift(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) (*ctrl.Result, error) { //nolint:unparam
	expected, ok := r.expectedReplicas(workload)
	if !ok {
		return nil, nil
	}

	actual := replicasFromTarget(target)

	if actual == expected {
		return nil, nil
	}

	logger := log.FromContext(ctx)
	logger.Info("replica drift detected", "expected", expected, "actual", actual)

	policy := resolveConflictAction(workload)
	metrics.DriftDetections.WithLabelValues(string(policy)).Inc()
	r.emitEvent(workload, false, "Warning", ReasonDriftDetected,
		"replicas changed externally from %d to %d, policy: %s", expected, actual, policy)

	switch policy {
	case v1alpha1.ConflictActionEnforce:
		if err := r.enforceReplicas(ctx, target, expected); err != nil {
			return nil, fmt.Errorf("enforcing replicas: %w", err)
		}
		r.stampLastActed(workload)
		if err := r.Status().Update(ctx, workload); err != nil {
			return nil, fmt.Errorf("updating status after drift correction: %w", err)
		}
		r.emitEvent(workload, false, "Normal", ReasonDriftCorrected,
			"replicas corrected from %d back to %d", actual, expected)

	case v1alpha1.ConflictActionDefer:
		r.acceptDrift(workload, actual)
		if err := r.Status().Update(ctx, workload); err != nil {
			return nil, fmt.Errorf("updating status after accepting drift: %w", err)
		}
	}

	return nil, nil
}

func (r *Reconciler) expectedReplicas(workload *v1alpha1.ManagedWorkload) (int32, bool) {
	switch workload.Status.Phase {
	case v1alpha1.PhasePaused:
		return 0, true
	case v1alpha1.PhaseRunning, v1alpha1.PhaseIdle, v1alpha1.PhaseScaling:
		if workload.Status.Scale != nil {
			return workload.Status.Scale.CurrentReplicas, true
		}
		return 0, false
	default:
		return 0, false
	}
}

func replicasFromTarget(obj client.Object) int32 {
	var replicas *int32
	switch t := obj.(type) {
	case *appsv1.Deployment:
		replicas = t.Spec.Replicas
	case *appsv1.StatefulSet:
		replicas = t.Spec.Replicas
	}
	if replicas == nil {
		return 1
	}
	return *replicas
}

func (r *Reconciler) enforceReplicas(ctx context.Context, target client.Object, desired int32) error {
	switch t := target.(type) {
	case *appsv1.Deployment:
		t.Spec.Replicas = &desired
	case *appsv1.StatefulSet:
		t.Spec.Replicas = &desired
	}
	return r.Update(ctx, target)
}

func (r *Reconciler) acceptDrift(workload *v1alpha1.ManagedWorkload, actual int32) {
	if workload.Status.Scale != nil {
		workload.Status.Scale.CurrentReplicas = actual
	}
	if workload.Status.Phase == v1alpha1.PhasePaused && actual > 0 {
		workload.Status.Pause = nil
		workload.Status.Phase = v1alpha1.PhaseRunning
	}
}

func (r *Reconciler) setCondition(workload *v1alpha1.ManagedWorkload, condType string, status metav1.ConditionStatus, reason, message string) {
	now := r.clockTime()
	for i, c := range workload.Status.Conditions {
		if c.Type == condType {
			if c.Status != status {
				workload.Status.Conditions[i].Status = status
				workload.Status.Conditions[i].Reason = reason
				workload.Status.Conditions[i].Message = message
				workload.Status.Conditions[i].LastTransitionTime = now
			}
			return
		}
	}
	workload.Status.Conditions = append(workload.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

func (r *Reconciler) findWorkloadsForTarget(ctx context.Context, obj client.Object) []reconcile.Request {
	var workloads v1alpha1.ManagedWorkloadList
	if err := r.List(ctx, &workloads, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, w := range workloads.Items {
		if w.Spec.Target.Name == obj.GetName() {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: w.Name, Namespace: w.Namespace},
			})
		}
	}
	return requests
}

// --- Helpers ---

func (r *Reconciler) transition(ctx context.Context, workload *v1alpha1.ManagedWorkload, phase v1alpha1.WorkloadPhase, reason string) (ctrl.Result, error) { //nolint:unparam
	logger := log.FromContext(ctx)
	old := workload.Status.Phase
	workload.Status.Phase = phase

	now := r.clockTime()
	workload.Status.LastTransitionTime = &now

	if err := r.Status().Update(ctx, workload); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating phase to %s: %w", phase, err)
	}
	metrics.LifecycleTransitions.WithLabelValues(string(old), string(phase)).Inc()
	logger.Info("phase transition", "from", old, "to", phase, "reason", reason)
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

func (r *Reconciler) observeActionDuration(workload *v1alpha1.ManagedWorkload, action string) {
	if workload.Status.LastTransitionTime == nil {
		return
	}
	duration := r.now().Sub(workload.Status.LastTransitionTime.Time).Seconds()
	metrics.LifecycleActionDuration.WithLabelValues(action).Observe(duration)
}

func (r *Reconciler) initDefaults() {
	if r.pauser == nil {
		r.pauser = lifecycle.NewPauser(r.Client)
	}
	if r.destroyer == nil {
		r.destroyer = lifecycle.NewDestroyer(r.Client)
	}
	if r.lifecycleScaler == nil {
		r.lifecycleScaler = lifecycle.NewScaler(r.Client)
	}
	if r.metrics == nil {
		r.metrics = metrics.NewReader(r.Client)
	}
	if r.engines == nil {
		r.engines = newEngineRegistry(func(threshold int) forecaster {
			return forecast.NewEngine(forecast.DefaultParams(), threshold)
		})
	}
	r.prometheusURL = r.PrometheusURL
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.initDefaults()
	targetHandler := handler.EnqueueRequestsFromMapFunc(r.findWorkloadsForTarget)
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ManagedWorkload{}).
		Watches(&appsv1.Deployment{}, targetHandler).
		Watches(&appsv1.StatefulSet{}, targetHandler).
		Named("managedworkload").
		Complete(r)
}
