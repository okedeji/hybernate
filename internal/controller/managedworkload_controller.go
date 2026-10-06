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
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/autoscaler"
	"github.com/okedeji/hybernate/internal/forecast"
	"github.com/okedeji/hybernate/internal/lifecycle"
	"github.com/okedeji/hybernate/internal/metrics"
)

const (
	finalizerName            = v1alpha1.FinalizerCleanup
	conditionDuplicateTarget = "DuplicateTarget"

	// duplicateRecheckInterval is a safety net for a blocked duplicate. Owner
	// deletion re-triggers it immediately via the sibling watch, but an owner
	// that is retargeted only emits an event carrying its new target.
	duplicateRecheckInterval = 5 * time.Minute

	// targetRecheckInterval is how often a workload whose target is missing
	// or ignored is looked at again, as a fallback to the target watch.
	targetRecheckInterval = time.Minute

	// reconcileTimeout bounds every call one reconcile makes, so a Kubernetes,
	// metrics or Prometheus endpoint that stops answering can't hold a worker,
	// and every workload queued behind it, forever.
	reconcileTimeout = 2 * time.Minute

	conditionWouldPause     = "WouldPause"
	conditionManualOverride = "ManualOverride"

	reasonDryRunWake = "DryRunWake"
)

// Reconciler drives ManagedWorkload objects through their lifecycle.
type Reconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Recorder      events.EventRecorder
	PrometheusURL string

	// MaxConcurrentReconciles is how many workloads are reconciled at once.
	// Zero means one.
	MaxConcurrentReconciles int

	// DoormanService and DoormanNamespace locate the doorman, whose Ready
	// pods paused workloads' Services are routed to. Empty disables it.
	DoormanService   string
	DoormanNamespace string

	// PodReader reads from the API server what the cache leaves out: pods,
	// Services' own EndpointSlices, and every workload's doorman routes.
	// Defaults to Client.
	PodReader client.Reader

	// ProtectedNamespaces are name patterns, such as prod-*, of namespaces
	// Hybernate doesn't manage unless they're labelled to allow it.
	ProtectedNamespaces []string

	// WatchNamespaces are the namespaces Hybernate works in, with a Role in
	// each, or every namespace when empty.
	WatchNamespaces []string

	// Timezone is where the forecast counts hours of the day and days of
	// the week, so that business hours stay in their slots through daylight
	// saving changes. Nil means UTC.
	Timezone *time.Location

	pauser          lifecyclePauser
	metrics         metricsReader
	prices          listPricer
	autoscalers     *autoscaler.Finder
	engines         *engineRegistry
	activityMemo    activityMemo
	doormanPorts    portAllocator
	doormanFailures failureBackoff
	prometheusURL   string
	clock           func() time.Time
}

type lifecyclePauser interface {
	Prepare(ctx context.Context, workload *v1alpha1.ManagedWorkload) error
	Pause(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error)
	Resume(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error)
	Restore(ctx context.Context, workload *v1alpha1.ManagedWorkload) (int32, error)
}

// +kubebuilder:rbac:groups=hybernate.io,resources=managedworkloads,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hybernate.io,resources=managedworkloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hybernate.io,resources=managedworkloads/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments/scale;statefulsets/scale,verbs=get;update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile evaluates the current state of a ManagedWorkload and acts on it.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, retErr error) {
	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()
	logger := log.FromContext(ctx)

	var doormanRetry time.Duration
	defer func() {
		res, retErr = retryIfStale(ctx, res, retErr)
		if retErr != nil {
			metrics.ReconcileErrors.WithLabelValues("managedworkload").Inc()
			return
		}
		if doormanRetry > 0 && (res.RequeueAfter == 0 || res.RequeueAfter > doormanRetry) {
			res.RequeueAfter = doormanRetry
		}
	}()

	var workload v1alpha1.ManagedWorkload
	if err := r.Get(ctx, req.NamespacedName, &workload); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !workload.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileDelete(ctx, &workload)
	}

	// Re-published every reconcile so the gauge is populated after an
	// operator restart, not only after the next transition.
	recordPhase(&workload)

	// The status as last written, to tell what this reconcile changed.
	observed := workload.Status.DeepCopy()

	if err := r.ensureFinalizer(ctx, &workload); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring finalizer: %w", err)
	}

	if duplicate, err := r.checkDuplicate(ctx, &workload); err != nil {
		return ctrl.Result{}, fmt.Errorf("checking duplicate target: %w", err)
	} else if duplicate {
		return r.reconcileDuplicate(ctx, &workload)
	}

	if workload.Status.Phase == "" || workload.Status.Phase == v1alpha1.PhaseCreating {
		return ctrl.Result{}, r.transition(ctx, &workload, v1alpha1.PhaseRunning, "Created")
	}

	target, err := r.checkTarget(ctx, &workload)
	if err != nil {
		return ctrl.Result{}, err
	}
	if target == nil {
		return r.reconcileTargetMissing(ctx, &workload)
	}
	if target.GetLabels()[v1alpha1.LabelIgnore] == v1alpha1.True {
		return r.reconcileIgnored(ctx, &workload)
	}
	r.setCondition(&workload, conditionTargetAvailable, metav1.ConditionTrue, "TargetExists", "")
	if err := r.wakeOnScaleUp(ctx, &workload, target); err != nil {
		return ctrl.Result{}, err
	}
	if protected, err := r.inProtectedNamespace(ctx, &workload); err != nil {
		return ctrl.Result{}, err
	} else if protected {
		return r.reconcileProtected(ctx, &workload)
	}
	r.clearCondition(&workload, conditionProtected, "NotProtected")
	if err := r.reportAutoscaler(ctx, &workload); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.learnDependencies(ctx, &workload, target); err != nil {
		return ctrl.Result{}, err
	}

	doormanRetry = r.routeDoorman(ctx, &workload, target)

	result, err := r.resumeTransition(ctx, &workload, target)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result != nil {
		return *result, nil
	}

	if atZero, err := r.scaledToZero(ctx, &workload, target); err != nil {
		return ctrl.Result{}, err
	} else if atZero {
		return r.reconcileScaledToZero(ctx, &workload, observed)
	}

	result, err = r.reconcileDesiredState(ctx, &workload, target)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result != nil {
		return *result, nil
	}

	result, err = r.reconcileAutomation(ctx, &workload, target)
	if err != nil {
		return ctrl.Result{}, err
	}

	r.trackDryRun(&workload)
	r.accumulateCost(ctx, &workload)
	if err := r.persistStatus(ctx, &workload, observed); err != nil {
		return ctrl.Result{}, err
	}

	logger.V(1).Info("reconciled", "workload", workload.Name, "namespace", workload.Namespace, "phase", workload.Status.Phase)
	if result != nil {
		return *result, nil
	}
	return ctrl.Result{}, nil
}

// resumeTransition finishes a pause or resume that an earlier reconcile
// started but didn't complete, without which a transient failure would strand
// the workload in the intermediate phase. It also turns back a pause the
// workload must no longer be in: dry-run never leaves a workload paused, and
// desiredState Running doesn't wait for a pause to finish first.
func (r *Reconciler) resumeTransition(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) (*ctrl.Result, error) {
	phase := workload.Status.Phase
	if workload.Spec.DryRun && (phase == v1alpha1.PhasePausing || phase == v1alpha1.PhasePaused) {
		return r.handleResume(ctx, workload, &phaseEvent{reason: reasonDryRunWake,
			message: "dry-run is on, so Hybernate wakes the workload it had paused"})
	}
	switch phase {
	case v1alpha1.PhasePausing:
		if desired := workload.Spec.DesiredState; desired != nil && *desired == v1alpha1.DesiredStateRunning {
			return r.handleResume(ctx, workload, nil)
		}
		return r.handlePause(ctx, workload, target, nil)
	case v1alpha1.PhaseResuming:
		return r.handleResume(ctx, workload, nil)
	default:
		return nil, nil
	}
}

func (r *Reconciler) reconcileDesiredState(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) (*ctrl.Result, error) {
	desired := workload.Spec.DesiredState
	if desired == nil || *desired != v1alpha1.DesiredStatePaused || !workload.Spec.DryRun {
		r.clearCondition(workload, conditionWouldPause, "NotHeldBack")
	}
	if desired == nil {
		return r.reconcilePauseRequest(ctx, workload, target)
	}

	switch *desired {
	case v1alpha1.DesiredStatePaused:
		if workload.Spec.DryRun {
			return nil, r.reportWouldPause(ctx, workload)
		}
		if until, held := gitOpsHold(workload, r.now()); held && workload.Status.Phase != v1alpha1.PhasePaused {
			return &ctrl.Result{RequeueAfter: until.Sub(r.now())}, nil
		}
		before := workload.Status.Phase
		result, err := r.handlePause(ctx, workload, target, nil)
		if err != nil {
			return nil, err
		}
		// Warned once the pause has begun: one held back, such as after a
		// GitOps tool undid the last, is tried again on every reconcile.
		if began := (before == v1alpha1.PhaseRunning || before == v1alpha1.PhaseIdle) &&
			workload.Status.Phase != before; began {
			if err := r.warnIfDependentsAwake(ctx, workload); err != nil {
				return nil, err
			}
		}
		return result, nil
	case v1alpha1.DesiredStateRunning:
		return r.handleResume(ctx, workload, nil)
	default:
		return nil, nil
	}
}

// reportWouldPause stands in for a desiredState pause under dry-run, which
// never takes an action that reduces availability.
func (r *Reconciler) reportWouldPause(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	if meta.IsStatusConditionTrue(workload.Status.Conditions, conditionWouldPause) {
		return nil
	}
	r.setCondition(workload, conditionWouldPause, metav1.ConditionTrue, "DryRun",
		"desiredState is Paused, but dry-run is on, so the workload is left running")
	if err := r.Status().Update(ctx, workload); err != nil {
		return fmt.Errorf("recording the pause dry-run held back: %w", err)
	}
	r.emitEvent(workload, true, "Normal", ReasonPaused, actionPause, "desiredState is Paused; would pause")
	return nil
}

// handlePause drives the workload to Paused. What the pause changes is
// recorded in the same status write that enters Pausing, before anything is
// scaled, so an interrupted pause is finished, or undone, from the record
// rather than from a target that's already at zero. why, if given, is
// announced once the workload is Pausing.
func (r *Reconciler) handlePause(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object,
	why *phaseEvent) (*ctrl.Result, error) {
	if workload.Spec.DryRun {
		return nil, nil
	}
	switch workload.Status.Phase {
	case v1alpha1.PhaseRunning, v1alpha1.PhaseIdle:
		if err := r.preparePause(ctx, workload, target); err != nil {
			return nil, err
		}
		// The cache showed replicas the target no longer has: it was scaled
		// to zero outside Hybernate, which the next reconcile sees.
		if workload.Status.Pause.PreviousReplicas == 0 {
			workload.Status.Pause = nil
			return &ctrl.Result{RequeueAfter: time.Second}, nil
		}
		if err := r.transition(ctx, workload, v1alpha1.PhasePausing, "PauseRequested"); err != nil {
			return nil, err
		}
		if why != nil {
			r.emitEvent(workload, false, "Normal", why.reason, actionPause, "%s", why.message)
		}
	case v1alpha1.PhasePausing:
		if workload.Status.Pause == nil {
			if err := r.preparePause(ctx, workload, target); err != nil {
				return nil, err
			}
			if err := r.Status().Update(ctx, workload); err != nil {
				return nil, fmt.Errorf("recording the pause: %w", err)
			}
		}
	default:
		return nil, nil
	}

	done, err := r.pauser.Pause(ctx, workload)
	if err != nil {
		return nil, fmt.Errorf("pausing workload: %w", err)
	}
	if !done {
		return &ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	r.stampLastActed(workload)
	took := r.sinceTransition(workload)
	if err := r.transition(ctx, workload, v1alpha1.PhasePaused, "Paused"); err != nil {
		return nil, err
	}
	metrics.LifecycleActionDuration.WithLabelValues("pause").Observe(took.Seconds())
	r.emitEvent(workload, false, "Normal", ReasonPaused, actionPause, "paused")
	return &ctrl.Result{RequeueAfter: r.pausedRecheck()}, nil
}

// preparePause records in workload.Status.Pause what the pause will change,
// for the caller to persist before acting on it.
func (r *Reconciler) preparePause(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) error {
	if err := r.pauser.Prepare(ctx, workload); err != nil {
		return fmt.Errorf("preparing to pause: %w", err)
	}
	workload.Status.Pause.Resources = r.captureResourceSnapshot(ctx, workload)
	workload.Status.Pause.WakeAnnotations = &v1alpha1.WakeAnnotations{
		Workload: activityAnnotationValues(workload),
		Target:   activityAnnotationValues(target),
	}
	return nil
}

// phaseEvent says why a workload pauses or wakes. It's emitted once the
// workload is Pausing or Resuming, so a retried pause or wake doesn't
// announce itself twice.
type phaseEvent struct {
	reason, message string
}

// handleResume drives the workload to Running: a paused one, or one pausing,
// is scaled back up and Running once its pods are Ready; an Idle one hasn't
// been paused yet and is simply Running again.
func (r *Reconciler) handleResume(ctx context.Context, workload *v1alpha1.ManagedWorkload, why *phaseEvent) (*ctrl.Result, error) {
	switch workload.Status.Phase {
	case v1alpha1.PhaseIdle:
		if err := r.transition(ctx, workload, v1alpha1.PhaseRunning, "ResumeRequested"); err != nil {
			return nil, err
		}
		return &ctrl.Result{}, nil
	case v1alpha1.PhasePausing, v1alpha1.PhasePaused:
		if workload.Status.Pause == nil {
			if err := r.pauser.Prepare(ctx, workload); err != nil {
				return nil, fmt.Errorf("recording what to restore: %w", err)
			}
			// A pause without its record was written by hand or by an older
			// version. Hybernate scaled it to zero, so zero isn't what it
			// had, and one is the least that runs it.
			workload.Status.Pause.PreviousReplicas = max(workload.Status.Pause.PreviousReplicas, 1)
		}
		// Learning never holds up a wake.
		if err := r.learnFromWake(ctx, workload); err != nil {
			log.FromContext(ctx).Info("couldn't learn what sent the request that woke the workload",
				"workload", workload.Name, "namespace", workload.Namespace, "error", err.Error())
		}
		// Woken before the phase changes, so a failure here is retried
		// from Paused rather than skipped by a retry that finds Resuming.
		if err := r.wakeDependencies(ctx, workload); err != nil {
			return nil, err
		}
		if err := r.transition(ctx, workload, v1alpha1.PhaseResuming, "ResumeRequested"); err != nil {
			return nil, err
		}
		if why != nil {
			r.emitEvent(workload, false, "Normal", why.reason, actionResume, "%s", why.message)
		}
	case v1alpha1.PhaseResuming:
	default:
		return nil, nil
	}

	if waiting, err := r.waitForDependencies(ctx, workload); waiting != nil || err != nil {
		return waiting, err
	}

	source := wakeSource(workload)
	done, err := r.pauser.Resume(ctx, workload)
	if err != nil {
		return nil, fmt.Errorf("resuming workload: %w", err)
	}
	if !done {
		return &ctrl.Result{RequeueAfter: resumeRecheck(r.sinceTransition(workload))}, nil
	}

	r.stampLastActed(workload)
	took := r.sinceTransition(workload)
	r.resetActivity(workload, source)
	tool, resolved := r.clearGitOpsConflict(workload)
	if err := r.transition(ctx, workload, v1alpha1.PhaseRunning, "Resumed"); err != nil {
		return nil, err
	}
	metrics.LifecycleActionDuration.WithLabelValues("resume").Observe(took.Seconds())
	if resolved {
		r.announceGitOpsConflictResolved(workload, tool)
	}
	r.emitEvent(workload, false, "Normal", ReasonResumed, actionResume, "resumed")
	return &ctrl.Result{}, nil
}

// resumeRecheck is when a resume waiting for its pods to be Ready is looked
// at again: after as long again as it has waited, from 5 seconds up to a
// minute. Pods becoming Ready requeue it through the target watch anyway,
// so this paces only pods that never do, each retry of which annotates the
// ScaledObject and reads the autoscalers and the scale.
func resumeRecheck(waited time.Duration) time.Duration {
	return min(max(waited, 5*time.Second), time.Minute)
}

func (r *Reconciler) ensureFinalizer(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	if controllerutil.ContainsFinalizer(workload, finalizerName) {
		return nil
	}
	controllerutil.AddFinalizer(workload, finalizerName)
	return r.Update(ctx, workload)
}

func (r *Reconciler) reconcileDelete(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	if !controllerutil.ContainsFinalizer(workload, finalizerName) {
		return nil
	}

	released, replicas, err := r.releaseTarget(ctx, workload)
	if err != nil {
		return err
	}

	controllerutil.RemoveFinalizer(workload, finalizerName)
	if err := r.Update(ctx, workload); err != nil {
		return fmt.Errorf("removing finalizer: %w", err)
	}
	if released {
		r.announceHandBack(workload, replicas, "no longer managed")
	}
	r.activityMemo.forget(workload.UID)
	r.doormanFailures.reset(workload.UID)
	r.forgetForecast(workload)
	metrics.DeleteWorkload(workload.Namespace, workload.Name)
	return nil
}

// releaseTarget is where Hybernate lets go of a target it stops managing: on
// deletion, the ignore label and a protected namespace. A paused target, or
// one being paused or resumed, is handed back as it was, scaled back up and
// released from KEDA without waiting for it to be Ready, and the dependencies
// it needs are woken. Its Services stop routing to the doorman. It reports
// whether there was a pause to undo, and the replicas the target is left
// with. The record is cleared by the caller's transition to Running, which
// settles the paused time's cost from it first.
func (r *Reconciler) releaseTarget(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, int32, error) {
	paused := workload.Status.Pause != nil
	var replicas int32
	if paused {
		var err error
		if replicas, err = r.pauser.Restore(ctx, workload); err != nil {
			return false, 0, fmt.Errorf("restoring the paused target: %w", err)
		}
		if err := r.wakeDependencies(ctx, workload); err != nil {
			return false, 0, err
		}
	}
	if err := r.removeDoorman(ctx, workload); err != nil {
		return false, 0, fmt.Errorf("removing doorman routes: %w", err)
	}
	return paused, replicas, nil
}

// announceHandBack says what handing a paused target back did, and why. A
// target that was at zero before its pause, or is gone, isn't scaled up.
func (r *Reconciler) announceHandBack(workload *v1alpha1.ManagedWorkload, replicas int32, why string) {
	if replicas == 0 {
		r.emitEvent(workload, false, "Normal", ReasonResumed, actionResume, "released at zero replicas: %s", why)
		return
	}
	r.emitEvent(workload, false, "Normal", ReasonResumed, actionResume, "restored to %d replicas: %s", replicas, why)
}

// reconcileDuplicate leaves a target another ManagedWorkload manages to
// that one, which the DuplicateTarget condition reports.
func (r *Reconciler) reconcileDuplicate(ctx context.Context, workload *v1alpha1.ManagedWorkload) (ctrl.Result, error) {
	c := meta.FindStatusCondition(workload.Status.Conditions, conditionDuplicateTarget)
	if err := r.refusePauseRequest(ctx, workload, "Warning", conditionDuplicateTarget, c.Message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: duplicateRecheckInterval}, nil
}

// reconcileIgnored stops managing a target labelled hybernate.io/ignore,
// restoring it first if it's paused, so the label never leaves it off.
func (r *Reconciler) reconcileIgnored(ctx context.Context, workload *v1alpha1.ManagedWorkload) (ctrl.Result, error) {
	ref := workload.Spec.Target
	reported := conditionFalseWith(workload, conditionTargetAvailable, "TargetIgnored")
	r.setCondition(workload, conditionTargetAvailable, metav1.ConditionFalse, "TargetIgnored",
		fmt.Sprintf("%s %s has %s label", ref.Kind, ref.Name, v1alpha1.LabelIgnore))
	if err := r.refusePauseRequest(ctx, workload, "Warning", "TargetIgnored", fmt.Sprintf(
		"%s %s has the %s label, so Hybernate doesn't manage it", ref.Kind, ref.Name, v1alpha1.LabelIgnore)); err != nil {
		return ctrl.Result{}, err
	}

	routed := len(workload.Status.Doorman) > 0
	released, replicas, err := r.releaseTarget(ctx, workload)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case workload.Status.Phase != v1alpha1.PhaseRunning || released:
		if err := r.transition(ctx, workload, v1alpha1.PhaseRunning, "TargetIgnored"); err != nil {
			return ctrl.Result{}, err
		}
	case !reported || routed:
		if err := r.Status().Update(ctx, workload); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating target condition: %w", err)
		}
	}
	if released {
		r.announceHandBack(workload, replicas, fmt.Sprintf("%s has the %s label", ref.Name, v1alpha1.LabelIgnore))
	}
	return ctrl.Result{RequeueAfter: targetRecheckInterval}, nil
}

// reconcileTargetMissing stops routing the workload's Services to the
// doorman while its target is gone: a request held there waits for pods
// that nothing will start, where without the doorman it fails at once.
func (r *Reconciler) reconcileTargetMissing(ctx context.Context, workload *v1alpha1.ManagedWorkload) (ctrl.Result, error) {
	ref := workload.Spec.Target
	if err := r.refusePauseRequest(ctx, workload, "Warning", ReasonTargetNotFound,
		fmt.Sprintf("%s %s not found", ref.Kind, ref.Name)); err != nil {
		return ctrl.Result{}, err
	}
	routed := len(workload.Status.Doorman) > 0
	if err := r.removeDoorman(ctx, workload); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing doorman routes: %w", err)
	}
	if routed {
		r.clearCondition(workload, conditionWakeOnRequest, "TargetNotFound")
		if err := r.Status().Update(ctx, workload); err != nil {
			return ctrl.Result{}, fmt.Errorf("recording doorman routes removed: %w", err)
		}
	}
	return ctrl.Result{RequeueAfter: targetRecheckInterval}, nil
}

func recordPhase(workload *v1alpha1.ManagedWorkload) {
	if workload.Status.Phase == "" {
		return
	}
	metrics.WorkloadPhase.WithLabelValues(workload.Namespace, workload.Name, string(workload.Status.Phase)).Set(1)
}

const (
	conditionTargetAvailable  = "TargetAvailable"
	conditionMetricsAvailable = "MetricsAvailable"
	// conditionPrometheusAvailable is only set when Prometheus activity
	// queries are configured.
	conditionPrometheusAvailable = "PrometheusAvailable"
)

// checkTarget returns the target workload, or nil when it doesn't exist,
// which the TargetAvailable condition reports.
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
		if conditionFalseWith(workload, conditionTargetAvailable, "TargetNotFound") {
			return nil, nil
		}
		r.setCondition(workload, conditionTargetAvailable, metav1.ConditionFalse, "TargetNotFound",
			fmt.Sprintf("%s %s not found", ref.Kind, ref.Name))
		if err := r.Status().Update(ctx, workload); err != nil {
			return nil, fmt.Errorf("updating target condition: %w", err)
		}
		r.emitEvent(workload, false, "Warning", ReasonTargetNotFound, actionCheckTarget,
			"%s %s not found", ref.Kind, ref.Name)
		metrics.TargetUnavailable.WithLabelValues(workload.Namespace, workload.Name).Inc()
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checking target %s %s: %w", ref.Kind, ref.Name, err)
	}
	return obj, nil
}

// conditionFalseWith reports whether a problem has already been reported,
// as the condition False for that reason.
func conditionFalseWith(workload *v1alpha1.ManagedWorkload, condType, reason string) bool {
	c := meta.FindStatusCondition(workload.Status.Conditions, condType)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == reason
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

func (r *Reconciler) setCondition(workload *v1alpha1.ManagedWorkload, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&workload.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: r.clockTime(),
	})
}

// checkDuplicate returns true if another ManagedWorkload in the same namespace
// already targets the same workload. The older resource (by creation time) wins;
// the newer one gets a DuplicateTarget condition and is skipped.
func (r *Reconciler) checkDuplicate(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error) {
	var list v1alpha1.ManagedWorkloadList
	if err := r.List(ctx, &list, client.InNamespace(workload.Namespace)); err != nil {
		return false, fmt.Errorf("listing managed workloads: %w", err)
	}

	for i := range list.Items {
		other := &list.Items[i]
		if other.UID == workload.UID || !other.DeletionTimestamp.IsZero() {
			continue
		}
		if other.Spec.Target != workload.Spec.Target || !claimsTargetFirst(other, workload) {
			continue
		}

		msg := fmt.Sprintf("%s/%s is already managed by %s", workload.Spec.Target.Kind, workload.Spec.Target.Name, other.Name)
		previous := meta.FindStatusCondition(workload.Status.Conditions, conditionDuplicateTarget)
		alreadyFlagged := previous != nil && previous.Status == metav1.ConditionTrue
		if alreadyFlagged && previous.Message == msg {
			return true, nil
		}
		r.setCondition(workload, conditionDuplicateTarget, metav1.ConditionTrue, conditionDuplicateTarget, msg)
		if err := r.Status().Update(ctx, workload); err != nil {
			return false, fmt.Errorf("updating duplicate condition: %w", err)
		}
		if !alreadyFlagged {
			r.Recorder.Eventf(workload, nil, "Warning", conditionDuplicateTarget, actionCheckDuplicate, "%s", msg)
		}
		return true, nil
	}

	// Clear the condition if it was previously set and the conflict is gone.
	for i, c := range workload.Status.Conditions {
		if c.Type == conditionDuplicateTarget && c.Status == metav1.ConditionTrue {
			workload.Status.Conditions[i].Status = metav1.ConditionFalse
			workload.Status.Conditions[i].Reason = "Resolved"
			workload.Status.Conditions[i].Message = ""
			workload.Status.Conditions[i].LastTransitionTime = r.clockTime()
			if err := r.Status().Update(ctx, workload); err != nil {
				return false, fmt.Errorf("clearing duplicate condition: %w", err)
			}
			break
		}
	}

	return false, nil
}

// claimsTargetFirst reports whether a owns a shared target ahead of b. The
// oldest ManagedWorkload wins. Creation timestamps have one-second precision,
// so UID breaks ties between objects created in the same second.
func claimsTargetFirst(a, b *v1alpha1.ManagedWorkload) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.UID < b.UID
}

// findRelatedWorkloads enqueues the ManagedWorkloads affected by a change to
// another: those sharing its target, so a blocked duplicate takes over when
// the owner is deleted; its dependencies, so they re-check the hold when it
// pauses; and its dependents, so they re-check when it wakes.
func (r *Reconciler) findRelatedWorkloads(ctx context.Context, obj client.Object) []reconcile.Request {
	changed, ok := obj.(*v1alpha1.ManagedWorkload)
	if !ok {
		return nil
	}

	var list v1alpha1.ManagedWorkloadList
	if err := r.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "listing related managed workloads",
			"workload", changed.Name, "namespace", changed.Namespace)
		return nil
	}

	dependsOn := map[workloadID]bool{}
	for _, ref := range dependencyRefs(changed) {
		dependsOn[dependencyID(changed, ref)] = true
	}
	changedTarget := targetID(changed)

	var requests []reconcile.Request
	for i := range list.Items {
		w := &list.Items[i]
		if w.UID == changed.UID {
			continue
		}
		if targetID(w) == changedTarget || dependsOn[targetID(w)] || dependsOnTarget(w, changedTarget) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: w.Name, Namespace: w.Namespace},
			})
		}
	}
	return requests
}

// findRoutedNeighbours enqueues the paused workloads in a namespace whose
// Services the doorman routes, when another workload there changes phase.
// One that wakes may share a Service with them, which its pods now serve,
// so they stop routing it at once rather than at their next recheck.
func (r *Reconciler) findRoutedNeighbours(ctx context.Context, obj client.Object) []reconcile.Request {
	var list v1alpha1.ManagedWorkloadList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "listing workloads sharing a namespace",
			"workload", obj.GetName(), "namespace", obj.GetNamespace())
		return nil
	}
	var requests []reconcile.Request
	for i := range list.Items {
		w := &list.Items[i]
		if w.UID != obj.GetUID() && w.Status.Phase == v1alpha1.PhasePaused && len(w.Status.Doorman) > 0 {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(w)})
		}
	}
	return requests
}

// phaseChanged passes a ManagedWorkload's updates that change its phase.
func phaseChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			old, okOld := e.ObjectOld.(*v1alpha1.ManagedWorkload)
			cur, okNew := e.ObjectNew.(*v1alpha1.ManagedWorkload)
			return okOld && okNew && old.Status.Phase != cur.Status.Phase
		},
	}
}

// targetRefFor identifies a watched object as a ManagedWorkload target.
// Typed objects from the cache carry no GVK, so the kind comes from the Go
// type rather than obj.GetObjectKind().
func targetRefFor(obj client.Object) (v1alpha1.WorkloadRef, bool) {
	switch obj.(type) {
	case *appsv1.Deployment:
		return v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: obj.GetName()}, true
	case *appsv1.StatefulSet:
		return v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindStatefulSet, Name: obj.GetName()}, true
	default:
		return v1alpha1.WorkloadRef{}, false
	}
}

// findWorkloadsForTarget enqueues the ManagedWorkloads that manage a changed
// Deployment or StatefulSet, and those that depend on it, so a resume waiting
// for the dependency to become Ready re-checks promptly.
func (r *Reconciler) findWorkloadsForTarget(ctx context.Context, obj client.Object) []reconcile.Request {
	target, ok := targetRefFor(obj)
	if !ok {
		return nil
	}
	id := workloadID{namespace: obj.GetNamespace(), kind: target.Kind, name: target.Name}

	var workloads v1alpha1.ManagedWorkloadList
	if err := r.List(ctx, &workloads); err != nil {
		log.FromContext(ctx).Error(err, "listing managed workloads for target",
			"target", target.Name, "kind", target.Kind, "namespace", obj.GetNamespace())
		return nil
	}

	var requests []reconcile.Request
	for i := range workloads.Items {
		w := &workloads.Items[i]
		if targetID(w) == id || dependsOnTarget(w, id) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: w.Name, Namespace: w.Namespace},
			})
		}
	}
	return requests
}

func dependsOnTarget(workload *v1alpha1.ManagedWorkload, id workloadID) bool {
	for _, ref := range dependencyRefs(workload) {
		if dependencyID(workload, ref) == id {
			return true
		}
	}
	return false
}

func (r *Reconciler) transition(ctx context.Context, workload *v1alpha1.ManagedWorkload, phase v1alpha1.WorkloadPhase, reason string) error {
	logger := log.FromContext(ctx)
	r.settleCost(ctx, workload)
	if phase == v1alpha1.PhaseRunning {
		workload.Status.Pause = nil
	}
	old := workload.Status.Phase
	workload.Status.Phase = phase

	now := r.clockTime()
	workload.Status.LastTransitionTime = &now

	if err := r.Status().Update(ctx, workload); err != nil {
		return fmt.Errorf("updating phase to %s: %w", phase, err)
	}
	metrics.WorkloadPhase.DeleteLabelValues(workload.Namespace, workload.Name, string(old))
	recordPhase(workload)
	metrics.LifecycleTransitions.WithLabelValues(string(old), string(phase)).Inc()
	logger.Info("phase transition", "from", old, "to", phase, "reason", reason)
	return nil
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

// sinceTransition is how long the workload has been in its phase.
func (r *Reconciler) sinceTransition(workload *v1alpha1.ManagedWorkload) time.Duration {
	if workload.Status.LastTransitionTime == nil {
		return 0
	}
	return r.now().Sub(workload.Status.LastTransitionTime.Time)
}

func (r *Reconciler) initDefaults() {
	if r.autoscalers == nil {
		r.autoscalers = autoscaler.NewFinder(r.Client)
	}
	if r.pauser == nil {
		r.pauser = lifecycle.NewPauser(r.Client, r.autoscalers)
	}
	if r.metrics == nil {
		pods := r.PodReader
		if pods == nil {
			pods = r.Client
		}
		reader := metrics.NewReader(r.Client, pods)
		r.metrics = reader
		if r.prices == nil {
			r.prices = reader
		}
	}
	if r.engines == nil {
		r.engines = newEngineRegistry(func() forecaster {
			return forecast.NewEngine(forecast.DefaultParams(), forecast.Settings{})
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
		Watches(&v1alpha1.ManagedWorkload{}, handler.EnqueueRequestsFromMapFunc(r.findRelatedWorkloads)).
		Watches(&v1alpha1.ManagedWorkload{}, handler.EnqueueRequestsFromMapFunc(r.findRoutedNeighbours),
			builder.WithPredicates(phaseChanged())).
		Watches(&appsv1.Deployment{}, targetHandler).
		Watches(&appsv1.StatefulSet{}, targetHandler).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(r.findWorkloadsForDoorman),
			builder.WithPredicates(predicate.NewPredicateFuncs(r.isDoormanEndpoints))).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.findPausedWorkloadsInNamespace)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.findWorkloadsInNamespace),
			builder.WithPredicates(inWatchedNamespaces(r.WatchNamespaces))).
		Named("managedworkload").
		WithOptions(controller.Options{MaxConcurrentReconciles: max(r.MaxConcurrentReconciles, 1)}).
		Complete(r)
}
