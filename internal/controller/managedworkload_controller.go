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
	"maps"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue"
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
	"github.com/okedeji/hybernate/internal/signal"
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

	// DoormanPortsPerWorkload and DoormanPortsPerNamespace cap the doorman
	// ports one workload, and one namespace, may hold. Zero means
	// DefaultDoormanPortsPerWorkload and DefaultDoormanPortsPerNamespace.
	DoormanPortsPerWorkload  int
	DoormanPortsPerNamespace int

	// PodReader reads from the API server what the cache leaves out: pods,
	// Services' own EndpointSlices, and every workload's doorman routes.
	// Defaults to Client.
	PodReader client.Reader

	// ProtectedNamespaces are name patterns, such as prod-*, of namespaces
	// Hybernate doesn't manage unless they're labelled to allow it.
	ProtectedNamespaces []string

	// HybernateNamespaces are the namespaces Hybernate itself runs in. Its
	// operator's and doorman's Deployments there are never paused, whatever
	// a ManagedWorkload or a namespace label says.
	HybernateNamespaces []string

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
	querySeries     querySeries
	activityWarned  annotationWarnings
	templateHashes  templateHashes
	phaseWrites     phaseWrites
	doormanPorts    portAllocator
	doormanFailures failureBackoff
	prometheus      *signal.Prometheus
	doormanFamilies doormanFamilies
	uids            workloadUIDs
	clock           func() time.Time
}

type lifecyclePauser interface {
	Prepare(ctx context.Context, workload *v1alpha1.ManagedWorkload) error
	Pause(ctx context.Context, workload *v1alpha1.ManagedWorkload) (lifecycle.Suspension, error)
	Resume(ctx context.Context, workload *v1alpha1.ManagedWorkload, waited lifecycle.Waited) (lifecycle.Resumption, error)
	Restore(ctx context.Context, workload *v1alpha1.ManagedWorkload) (int32, error)
}

// +kubebuilder:rbac:groups=hybernate.io,resources=managedworkloads,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hybernate.io,resources=managedworkloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hybernate.io,resources=managedworkloads/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;patch
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

	var workload v1alpha1.ManagedWorkload
	var doormanRetry time.Duration
	defer func() {
		if retErr == nil {
			if doormanRetry > 0 && (res.RequeueAfter == 0 || res.RequeueAfter > doormanRetry) {
				res.RequeueAfter = doormanRetry
			}
			res.Priority = requeuePriority(&workload, res)
		}
		// A write that lost to a newer one is retried at the priority of what
		// brought it, such as a wake.
		res, retErr = retryIfStale(ctx, res, retErr)
		if retErr != nil {
			metrics.ReconcileErrors.WithLabelValues("managedworkload").Inc()
		}
	}()

	if found, err := r.readWorkload(ctx, req.NamespacedName, &workload); err != nil || !found {
		return ctrl.Result{}, err
	}

	if !workload.DeletionTimestamp.IsZero() {
		wait, err := r.reconcileDelete(ctx, &workload)
		return ctrl.Result{RequeueAfter: wait}, err
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
	if result, err := r.recoverPause(ctx, &workload, target); err != nil {
		return ctrl.Result{}, err
	} else if result != nil {
		return *result, nil
	}
	if target.GetLabels()[v1alpha1.LabelIgnore] == v1alpha1.True {
		return r.reconcileIgnored(ctx, &workload)
	}
	r.setCondition(&workload, conditionTargetAvailable, metav1.ConditionTrue, "TargetExists", "")
	if p, err := r.protectionOf(ctx, &workload, target); err != nil {
		return ctrl.Result{}, err
	} else if p != nil {
		return r.reconcileProtected(ctx, &workload, *p)
	}
	r.clearCondition(&workload, conditionProtected, "NotProtected")
	if err := r.observeTarget(ctx, &workload, target); err != nil {
		return ctrl.Result{}, err
	}

	if stale, err := r.staleForRouting(ctx, &workload); err != nil {
		return ctrl.Result{}, err
	} else if stale {
		return ctrl.Result{RequeueAfter: staleRetry}, nil
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

	result, err = r.reconcilePauseRequest(ctx, &workload, target)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result != nil {
		return *result, nil
	}

	return r.reconcileSettled(ctx, &workload, target, observed)
}

// Every workload's reconciles share one queue, ordered by priority, so a
// wake, which a held request or a person waits on, isn't queued behind the
// periodic checks of every awake workload, each of which may read metrics
// and Prometheus. Events come at the queue's default priority, ahead of
// those checks, and a request, an activity or pause annotation, or a
// scale-up that wakes a workload ahead of other events.
const priorityWake = 100

// requeuePriority is the priority a workload's next reconcile is queued at:
// the wake's while it resumes, its own for the rest of a pause, and below
// any event's for a periodic check.
func requeuePriority(workload *v1alpha1.ManagedWorkload, res ctrl.Result) *int {
	if res.Priority != nil || res.RequeueAfter == 0 {
		return res.Priority
	}
	switch workload.Status.Phase {
	case v1alpha1.PhaseResuming:
		return ptr.To(priorityWake)
	case v1alpha1.PhasePausing:
		return nil
	default:
		return ptr.To(handler.LowPriority)
	}
}

// enqueueAt adds requests to the queue at priority, where it's a priority
// queue.
func enqueueAt(q workqueue.TypedRateLimitingInterface[reconcile.Request], priority int, requests ...reconcile.Request) {
	if pq, ok := q.(priorityqueue.PriorityQueue[reconcile.Request]); ok {
		pq.AddWithOpts(priorityqueue.AddOpts{Priority: ptr.To(priority)}, requests...)
		return
	}
	for _, req := range requests {
		q.Add(req)
	}
}

// wakeHandler enqueues at priorityWake a ManagedWorkload whose annotations
// changed, as the doorman's stamp of a held request, kubectl hybernate wake
// and pause, and other tools' activity annotations change them.
func wakeHandler() handler.EventHandler {
	return handler.Funcs{
		UpdateFunc: func(_ context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			if !maps.Equal(e.ObjectOld.GetAnnotations(), e.ObjectNew.GetAnnotations()) {
				enqueueAt(q, priorityWake, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(e.ObjectNew)})
			}
		},
	}
}

// scaleUpHandler enqueues at priorityWake the ManagedWorkloads of a
// Deployment or StatefulSet scaled up from zero, which wakes one paused.
func (r *Reconciler) scaleUpHandler() handler.EventHandler {
	return handler.Funcs{
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			target, ok := targetRefFor(e.ObjectNew)
			if !ok || replicasFromTarget(e.ObjectOld) > 0 || replicasFromTarget(e.ObjectNew) == 0 {
				return
			}
			enqueueAt(q, priorityWake, r.workloadsBy(ctx, "", client.InNamespace(e.ObjectNew.GetNamespace()),
				client.MatchingFields{targetField: targetKey(target.Kind, target.Name)})...)
		},
	}
}

// observeTarget takes in what's changed on the target Hybernate manages:
// a scale-up outside it, and the autoscaler that scales it.
func (r *Reconciler) observeTarget(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) error {
	if err := r.wakeOnScaleUp(ctx, workload, target); err != nil {
		return err
	}
	return r.reportAutoscaler(ctx, workload)
}

// readWorkload reads the workload, and reports whether it exists. One
// deleted without its finalizer running would otherwise leave its metric
// series and what's kept in memory for it, and one created again under the
// same name what was kept for the one before.
func (r *Reconciler) readWorkload(ctx context.Context, name types.NamespacedName,
	workload *v1alpha1.ManagedWorkload) (bool, error) {
	err := r.Get(ctx, name, workload)
	if apierrors.IsNotFound(err) {
		if uid, ok := r.uids.take(name); ok {
			r.forgetWorkload(uid)
		}
		metrics.DeleteWorkload(name.Namespace, name.Name)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting managed workload: %w", err)
	}
	if replaced, ok := r.uids.see(name, workload.UID); ok {
		r.forgetWorkload(replaced)
	}
	return true, nil
}

// reconcileSettled runs automation on a workload no pause or resume is
// under way for, and writes its status if that changed it.
func (r *Reconciler) reconcileSettled(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object,
	observed *v1alpha1.ManagedWorkloadStatus) (ctrl.Result, error) {
	allReady := r.clearResumeIncomplete(workload, target)
	r.clearPauseIncomplete(workload, target)
	result, err := r.reconcileAutomation(ctx, workload, target)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Learned once the wake or pause is under way, so reading the
	// environment never holds one up, and a failure to is only retried.
	if err := r.learnDependencies(ctx, workload, target); err != nil {
		log.FromContext(ctx).Info("couldn't learn the workload's dependencies", "workload", workload.Name,
			"namespace", workload.Namespace, "error", err.Error())
	}

	r.trackDryRun(workload)
	r.accumulateCost(ctx, workload)
	if err := r.persistStatus(ctx, workload, observed); err != nil {
		return ctrl.Result{}, err
	}
	// Announced once written, so a write that fails isn't announced twice.
	if allReady {
		r.emitEvent(workload, false, "Normal", ReasonAllReplicasReady, actionResume,
			"all %d replicas are Ready, after the last resume stopped waiting for them", lifecycle.ReadyReplicas(target))
	}

	log.FromContext(ctx).V(1).Info("reconciled", "workload", workload.Name, "namespace", workload.Namespace,
		"phase", workload.Status.Phase)
	if result != nil {
		return *result, nil
	}
	return ctrl.Result{}, nil
}

// staleForRouting reports whether the workload's phase, as read from the
// cache, is out of date for routing it to the doorman. A cache that hasn't
// yet seen a resume finish still holds the workload Paused, and routing
// that would put back the doorman slices the resume just removed. Only the
// API server can tell, and only a workload that would be routed, and whose
// phase this operator has just written, needs asking: the cache can only be
// behind a write, and every paused workload is reconciled every few
// minutes, which would otherwise read each one past the cache each time.
func (r *Reconciler) staleForRouting(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error) {
	if !doormanEligible(workload) || !r.phaseWrites.recent(workload.UID, r.now()) {
		return false, nil
	}
	phase, err := r.livePhase(ctx, workload)
	if err != nil {
		return false, err
	}
	return phase != workload.Status.Phase, nil
}

// resumeTransition finishes a pause or resume that an earlier reconcile
// started but didn't complete, without which a transient failure would strand
// the workload in the intermediate phase. It also turns back a pause the
// workload must no longer be in: dry-run never leaves a workload paused.
func (r *Reconciler) resumeTransition(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) (*ctrl.Result, error) {
	phase := workload.Status.Phase
	if workload.Spec.DryRun && (phase == v1alpha1.PhasePausing || phase == v1alpha1.PhasePaused) {
		return r.handleResume(ctx, workload, &phaseEvent{reason: reasonDryRunWake,
			message: "dry-run is on, so Hybernate wakes the workload it had paused"})
	}
	switch phase {
	case v1alpha1.PhasePausing:
		return r.handlePause(ctx, workload, target, nil)
	case v1alpha1.PhaseResuming:
		return r.handleResume(ctx, workload, nil)
	default:
		return nil, nil
	}
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
	default:
		return nil, nil
	}

	scaledDown := workload.Status.Pause.PausedAt != nil
	paused, err := r.pauser.Pause(ctx, workload)
	if err != nil {
		return nil, fmt.Errorf("pausing workload: %w", err)
	}
	if !paused.Done {
		// When it was scaled to zero is written as it's first scaled, so the
		// wait for its pods, and a scale-up meanwhile, outlive a restart.
		if !scaledDown {
			if err := r.Status().Update(ctx, workload); err != nil {
				return nil, fmt.Errorf("recording the workload scaled to zero: %w", err)
			}
		}
		return &ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	r.stampLastActed(workload)
	took := r.sinceTransition(workload)
	var stillRunning string
	if paused.Running > 0 {
		stillRunning = fmt.Sprintf("paused with %d pods still running %s after it was scaled to zero: they may be "+
			"stuck terminating, or a StatefulSet with OrderedReady pod management may be waiting on one that isn't "+
			"Ready", paused.Running, roundedDuration(r.now().Sub(workload.Status.Pause.PausedAt.Time)))
		r.setCondition(workload, conditionPauseIncomplete, metav1.ConditionTrue, "PodsStillRunning", stillRunning)
	} else if meta.IsStatusConditionTrue(workload.Status.Conditions, conditionPauseIncomplete) {
		r.setCondition(workload, conditionPauseIncomplete, metav1.ConditionFalse, reasonPodsStopped, "")
	}
	if err := r.transition(ctx, workload, v1alpha1.PhasePaused, "Paused"); err != nil {
		return nil, err
	}
	metrics.LifecycleActionDuration.WithLabelValues("pause").Observe(took.Seconds())
	if stillRunning != "" {
		r.emitEvent(workload, false, "Warning", ReasonPauseIncomplete, actionPause, "%s", stillRunning)
	} else {
		r.emitEvent(workload, false, "Normal", ReasonPaused, actionPause, "paused")
	}
	return &ctrl.Result{RequeueAfter: r.pausedRecheck()}, nil
}

// conditionPauseIncomplete says the workload's last pause stopped waiting
// for its pods with some still running.
const (
	conditionPauseIncomplete = "PauseIncomplete"

	ReasonPauseIncomplete = "PauseIncomplete"
	reasonPodsStopped     = "PodsStopped"
)

// clearPauseIncomplete sets PauseIncomplete False once the pods its last
// pause stopped waiting for are gone, or the workload is awake again.
func (r *Reconciler) clearPauseIncomplete(workload *v1alpha1.ManagedWorkload, target client.Object) {
	if !meta.IsStatusConditionTrue(workload.Status.Conditions, conditionPauseIncomplete) {
		return
	}
	switch {
	case workload.Status.Phase != v1alpha1.PhasePaused:
		r.setCondition(workload, conditionPauseIncomplete, metav1.ConditionFalse, "NotPaused", "")
	case lifecycle.Pods(target) == 0:
		r.setCondition(workload, conditionPauseIncomplete, metav1.ConditionFalse, reasonPodsStopped, "")
	}
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

const (
	ReasonPauseRecovered  = "PauseRecovered"
	ReasonPauseRecordLost = "PauseRecordLost"
)

// recoverPause takes up a pause whose record the workload's status has
// lost, as to a backup restored without status or a ManagedWorkload created
// afresh, from the copy its target carries. Without it, a target Hybernate
// paused would be taken for one scaled to zero by someone else and left off
// for good, with KEDA holding it there. A target still at zero is Paused
// again, to wake like any other; one scaled up since is released. With no
// copy on the target either, nothing was scaled, since the copy is written
// first, so a workload left pausing, paused or resuming is Running again.
func (r *Reconciler) recoverPause(ctx context.Context, workload *v1alpha1.ManagedWorkload,
	target client.Object) (*ctrl.Result, error) {
	if workload.Status.Pause != nil {
		return nil, nil
	}
	pause, err := r.pauseRecordOn(ctx, workload, target)
	if err != nil {
		return nil, err
	}
	if pause == nil {
		return r.forgetLostPause(ctx, workload, "nothing was scaled, so there's nothing to restore")
	}

	replicas, err := r.liveReplicas(ctx, target)
	if err != nil {
		return nil, err
	}
	if replicas > 0 {
		workload.Status.Pause = pause
		if _, err := r.pauser.Restore(ctx, workload); err != nil {
			return nil, fmt.Errorf("releasing a target scaled up since its pause record was lost: %w", err)
		}
		workload.Status.Pause = nil
		return r.forgetLostPause(ctx, workload, fmt.Sprintf("it has %d replicas again, so it's released", replicas))
	}

	now := r.clockTime()
	pause.PausedAt = &now
	if snap := r.captureResourceSnapshot(ctx, workload); snap != nil {
		snap.Replicas = pause.PreviousReplicas
		pause.Resources = snap
	}
	pause.WakeAnnotations = &v1alpha1.WakeAnnotations{
		Workload: activityAnnotationValues(workload),
		Target:   activityAnnotationValues(target),
	}
	workload.Status.Pause = pause
	if err := r.transition(ctx, workload, v1alpha1.PhasePaused, ReasonPauseRecovered); err != nil {
		return nil, err
	}
	r.emitEvent(workload, false, "Normal", ReasonPauseRecovered, actionPause,
		"its pause record was lost, so it's taken up again from the %s annotation: paused, to wake to %d replicas",
		v1alpha1.AnnotationPausedFrom, pause.PreviousReplicas)
	return &ctrl.Result{RequeueAfter: r.pausedRecheck()}, nil
}

// pauseRecordOn is the copy of a pause record the target carries, read past
// the cache, which can still show a copy a resume has just removed. A copy
// that can't be read is reported and treated as none.
func (r *Reconciler) pauseRecordOn(ctx context.Context, workload *v1alpha1.ManagedWorkload,
	target client.Object) (*v1alpha1.PauseStatus, error) {
	if _, ok := target.GetAnnotations()[v1alpha1.AnnotationPausedFrom]; !ok {
		return nil, nil
	}
	live, ok := target.DeepCopyObject().(client.Object)
	if !ok {
		return nil, fmt.Errorf("copying %s %s", workload.Spec.Target.Kind, target.GetName())
	}
	if err := r.apiReader().Get(ctx, client.ObjectKeyFromObject(target), live); err != nil {
		return nil, fmt.Errorf("reading the target's pause record: %w", err)
	}
	pause, err := lifecycle.RecordOn(live)
	if err != nil {
		r.emitEvent(workload, false, "Warning", ReasonPauseRecordLost, actionPause, "%v", err)
		return nil, nil
	}
	return pause, nil
}

// forgetLostPause sets Running a workload left pausing, paused or resuming
// without a pause record to finish it from, saying why.
func (r *Reconciler) forgetLostPause(ctx context.Context, workload *v1alpha1.ManagedWorkload,
	outcome string) (*ctrl.Result, error) {
	switch workload.Status.Phase {
	case v1alpha1.PhasePausing, v1alpha1.PhasePaused, v1alpha1.PhaseResuming:
	default:
		return nil, nil
	}
	was := workload.Status.Phase
	if err := r.transition(ctx, workload, v1alpha1.PhaseRunning, ReasonPauseRecordLost); err != nil {
		return nil, err
	}
	r.emitEvent(workload, false, "Warning", ReasonPauseRecordLost, actionResume,
		"%s without a pause record to finish from: %s", strings.ToLower(string(was)), outcome)
	return &ctrl.Result{}, nil
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
		// Learning never holds up a wake.
		if err := r.learnFromWake(ctx, workload); err != nil {
			log.FromContext(ctx).Info("couldn't learn what sent the request that woke the workload",
				"workload", workload.Name, "namespace", workload.Namespace, "error", err.Error())
		}
		r.wakeDependencies(ctx, workload)
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

	before := meta.FindStatusCondition(workload.Status.Conditions, conditionWaitingForDependencies).DeepCopy()
	if waiting, err := r.waitForDependencies(ctx, workload); waiting != nil || err != nil {
		return waiting, err
	}
	// The wait for its own replicas is timed from when its dependencies
	// were Ready, which only that condition's change records. Unwritten, the
	// next reconcile would make it again, and the wait would start over.
	after := meta.FindStatusCondition(workload.Status.Conditions, conditionWaitingForDependencies)
	if conditionChanged(before, after) {
		if err := r.Status().Update(ctx, workload); err != nil {
			return nil, fmt.Errorf("recording its dependencies Ready: %w", err)
		}
		if after.Status == metav1.ConditionFalse && after.Reason != "DependenciesReady" {
			r.emitEvent(workload, false, "Warning", after.Reason, actionResume, "%s", after.Message)
		}
	}

	source := wakeSource(workload)
	waited := lifecycle.Waited{ScaledUp: r.sinceScaledUp(workload), Serving: r.sinceFirstReady(workload)}
	scaledUp := workload.Status.Pause != nil && workload.Status.Pause.ResumedAt != nil
	resumed, err := r.pauser.Resume(ctx, workload, waited)
	if err != nil {
		return nil, fmt.Errorf("resuming workload: %w", err)
	}
	if resumed.ScaledToZero {
		// Left Running, where the next reconcile finds it scaled to zero
		// outside Hybernate, says so, and leaves it off.
		if err := r.transition(ctx, workload, v1alpha1.PhaseRunning, ReasonScaledToZero); err != nil {
			return nil, err
		}
		log.FromContext(ctx).Info("resuming workload scaled to zero outside Hybernate, left off",
			"workload", workload.Name, "namespace", workload.Namespace)
		return &ctrl.Result{}, nil
	}
	if !resumed.Settled {
		if err := r.recordResumeProgress(ctx, workload, resumed, scaledUp); err != nil {
			return nil, err
		}
		return &ctrl.Result{RequeueAfter: resumeRecheck(waited.ScaledUp)}, nil
	}

	r.stampLastActed(workload)
	took := r.sinceTransition(workload)
	r.resetActivity(workload, source)
	tool, resolved := r.clearGitOpsConflict(workload)
	short := resumed.Ready < resumed.Replicas
	var shortfall string
	if short {
		shortfall = fmt.Sprintf("resumed with %d of %d replicas Ready: Hybernate stopped waiting for the rest after %s",
			resumed.Ready, resumed.Replicas, roundedDuration(waited.ScaledUp))
		r.setCondition(workload, conditionResumeIncomplete, metav1.ConditionTrue, "ReplicasNotReady", shortfall)
	} else {
		r.clearCondition(workload, conditionResumeIncomplete, ReasonAllReplicasReady)
	}
	if err := r.transition(ctx, workload, v1alpha1.PhaseRunning, "Resumed"); err != nil {
		return nil, err
	}
	metrics.LifecycleActionDuration.WithLabelValues("resume").Observe(took.Seconds())
	if resolved {
		r.announceGitOpsConflictResolved(workload, tool)
	}
	if short {
		r.emitEvent(workload, false, "Warning", ReasonResumeIncomplete, actionResume, "%s", shortfall)
	} else {
		r.emitEvent(workload, false, "Normal", ReasonResumed, actionResume, "resumed")
	}
	return &ctrl.Result{}, nil
}

// conditionResumeIncomplete says the workload's last resume ended with some
// of its replicas not Ready, after waiting as long as Hybernate waits.
const (
	conditionResumeIncomplete = "ResumeIncomplete"

	ReasonResumeIncomplete = "ResumeIncomplete"
	ReasonAllReplicasReady = "AllReplicasReady"
)

// clearResumeIncomplete sets ResumeIncomplete False once the replicas its
// last resume stopped waiting for are Ready after all, so it isn't reported
// as needing attention until the next resume.
func (r *Reconciler) clearResumeIncomplete(workload *v1alpha1.ManagedWorkload, target client.Object) bool {
	if !meta.IsStatusConditionTrue(workload.Status.Conditions, conditionResumeIncomplete) ||
		!lifecycle.IsReady(target) {
		return false
	}
	r.setCondition(workload, conditionResumeIncomplete, metav1.ConditionFalse, ReasonAllReplicasReady, "")
	return true
}

// sinceFirstReady is how long a resuming workload has had a replica Ready,
// as recordResumeProgress wrote when it first did, or zero.
func (r *Reconciler) sinceFirstReady(workload *v1alpha1.ManagedWorkload) time.Duration {
	if workload.Status.Pause == nil || workload.Status.Pause.FirstReadyAt == nil {
		return 0
	}
	return r.now().Sub(workload.Status.Pause.FirstReadyAt.Time)
}

// recordResumeProgress writes when a resume scaled the workload up, after
// which one found at zero was scaled there by someone else, and when it
// first saw one of its replicas Ready, from which the rest are given
// lifecycle.ServingWait. They're kept in the pause record, so both survive
// an operator restart. scaledUp is whether the scale-up was written before.
func (r *Reconciler) recordResumeProgress(ctx context.Context, workload *v1alpha1.ManagedWorkload,
	resumed lifecycle.Resumption, scaledUp bool) error {
	pause := workload.Status.Pause
	if pause == nil {
		return nil
	}
	changed := !scaledUp && pause.ResumedAt != nil
	if resumed.Ready > 0 && pause.FirstReadyAt == nil {
		now := r.clockTime()
		pause.FirstReadyAt = &now
		changed = true
	}
	if !changed {
		return nil
	}
	if err := r.Status().Update(ctx, workload); err != nil {
		return fmt.Errorf("recording the resume's progress: %w", err)
	}
	return nil
}

// sinceScaledUp is how long a resuming workload has waited for its own
// replicas: since it began resuming, or since the dependencies it waits for
// were Ready, if that was later.
func (r *Reconciler) sinceScaledUp(workload *v1alpha1.ManagedWorkload) time.Duration {
	waited := r.sinceTransition(workload)
	c := meta.FindStatusCondition(workload.Status.Conditions, conditionWaitingForDependencies)
	if c == nil || c.Status == metav1.ConditionTrue {
		return waited
	}
	return min(waited, r.now().Sub(c.LastTransitionTime.Time))
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

// reconcileDelete hands the target back and lets the ManagedWorkload go. It
// returns how long to wait first when a doorman port the workload used is
// still draining: the drain is kept in its status until it ends, so a leader
// that takes over meanwhile doesn't give the port to another workload while
// clients may still send this one's traffic to it.
func (r *Reconciler) reconcileDelete(ctx context.Context, workload *v1alpha1.ManagedWorkload) (time.Duration, error) {
	if !controllerutil.ContainsFinalizer(workload, finalizerName) {
		return 0, nil
	}

	released, replicas, err := r.releaseTarget(ctx, workload)
	if err != nil && !r.abandonHandBack(ctx, workload, err) {
		return 0, err
	}
	// Written with the drain, so the reconciles while it lasts don't hand
	// the target back again: scaling up one someone has since scaled to
	// zero, putting KEDA's value back, and waking its dependencies.
	workload.Status.Pause = nil
	if wait := r.drainLeft(workload); wait > 0 {
		if err := r.Status().Update(ctx, workload); err != nil {
			return 0, fmt.Errorf("recording the doorman ports draining: %w", err)
		}
		if released {
			r.announceHandBack(workload, replicas, "no longer managed")
		}
		return wait, nil
	}

	controllerutil.RemoveFinalizer(workload, finalizerName)
	if err := r.Update(ctx, workload); err != nil {
		return 0, fmt.Errorf("removing finalizer: %w", err)
	}
	if released {
		r.announceHandBack(workload, replicas, "no longer managed")
	}
	r.uids.take(client.ObjectKeyFromObject(workload))
	r.forgetWorkload(workload.UID)
	metrics.DeleteWorkload(workload.Namespace, workload.Name)
	return 0, nil
}

const (
	// handBackGiveUpAfter is how long a ManagedWorkload in a namespace being
	// deleted keeps retrying a hand-back that fails before it goes without
	// it. Its target goes with the namespace, so there's nothing left to hand
	// back to, and a hand-back that can't succeed, such as one a KEDA
	// webhook that's down rejects, would otherwise hold the namespace's
	// deletion forever.
	handBackGiveUpAfter = 5 * time.Minute

	ReasonHandBackAbandoned = "HandBackAbandoned"
)

// abandonHandBack reports whether a deletion whose hand-back failed should
// let the ManagedWorkload go anyway: only once its namespace is being
// deleted, and it has kept failing for handBackGiveUpAfter.
func (r *Reconciler) abandonHandBack(ctx context.Context, workload *v1alpha1.ManagedWorkload, failure error) bool {
	if r.now().Sub(workload.DeletionTimestamp.Time) < handBackGiveUpAfter {
		return false
	}
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: workload.Namespace}, &ns); err != nil {
		return false
	}
	if ns.DeletionTimestamp.IsZero() && ns.Status.Phase != corev1.NamespaceTerminating {
		return false
	}
	log.FromContext(ctx).Info("namespace is being deleted, letting the workload go without handing it back",
		"workload", workload.Name, "namespace", workload.Namespace, "error", failure.Error())
	r.emitEvent(workload, false, "Warning", ReasonHandBackAbandoned, actionResume,
		"namespace %s is being deleted and handing the target back has failed for %s, so Hybernate lets it go: %v",
		workload.Namespace, roundedDuration(handBackGiveUpAfter), failure)
	return true
}

// forgetWorkload drops what's kept in memory for a workload that's gone.
// Its doorman ports drain before another workload may have them.
func (r *Reconciler) forgetWorkload(uid types.UID) {
	r.activityMemo.forget(uid)
	r.querySeries.forget(uid)
	r.activityWarned.forget(uid)
	r.templateHashes.forget(uid)
	r.phaseWrites.forget(uid)
	r.doormanFailures.reset(uid)
	r.doormanPorts.release(uid, r.now())
	r.engines.forget(uid)
}

// workloadUIDs remembers the UID of each workload by name. A workload
// deleted without its finalizer running leaves only its name to reconcile,
// and one deleted and created again under the same name a new UID, while
// what's kept in memory for it is kept by UID.
type workloadUIDs struct {
	mu     sync.Mutex
	byName map[types.NamespacedName]types.UID
}

// see records the workload's UID, and returns the one it replaces, if any.
func (u *workloadUIDs) see(name types.NamespacedName, uid types.UID) (types.UID, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.byName == nil {
		u.byName = make(map[types.NamespacedName]types.UID)
	}
	previous, ok := u.byName[name]
	u.byName[name] = uid
	return previous, ok && previous != uid
}

// take forgets the workload's UID, returning it if it was known.
func (u *workloadUIDs) take(name types.NamespacedName) (types.UID, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	uid, ok := u.byName[name]
	delete(u.byName, name)
	return uid, ok
}

// drainLeft is how long until the last of the workload's doorman ports has
// drained.
func (r *Reconciler) drainLeft(workload *v1alpha1.ManagedWorkload) time.Duration {
	var left time.Duration
	for _, d := range workload.Status.DoormanDraining {
		left = max(left, d.Until.Sub(r.now()))
	}
	return left
}

// releaseTarget is where Hybernate lets go of a target it stops managing: on
// deletion, the ignore label and a protected namespace. A paused target, or
// one being paused or resumed, is handed back as it was, scaled back up and
// released from KEDA without waiting for it to be Ready, and the dependencies
// it needs are woken. Its Services stop routing to the doorman. It reports
// whether there was a pause to undo, and the replicas the target is left
// with. The record is cleared by the caller: by its transition to Running,
// which settles the paused time's cost from it first, or on deletion.
func (r *Reconciler) releaseTarget(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, int32, error) {
	paused := workload.Status.Pause != nil
	var replicas int32
	if paused {
		var err error
		if replicas, err = r.pauser.Restore(ctx, workload); err != nil {
			return false, 0, fmt.Errorf("restoring the paused target: %w", err)
		}
		r.wakeDependencies(ctx, workload)
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
// that one, which the DuplicateTarget condition reports. One that held the
// target until the other took it over hands back what it had paused, and
// its doorman routes, or nothing would ever wake the target.
func (r *Reconciler) reconcileDuplicate(ctx context.Context, workload *v1alpha1.ManagedWorkload) (ctrl.Result, error) {
	c := meta.FindStatusCondition(workload.Status.Conditions, conditionDuplicateTarget)
	if err := r.refusePauseRequest(ctx, workload, "Warning", conditionDuplicateTarget, c.Message); err != nil {
		return ctrl.Result{}, err
	}
	if workload.Status.Pause == nil && len(workload.Status.Doorman) == 0 {
		return ctrl.Result{RequeueAfter: duplicateRecheckInterval}, nil
	}

	released, replicas, err := r.releaseTarget(ctx, workload)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.clearCondition(workload, conditionWakeOnRequest, conditionDuplicateTarget)
	if err := r.transition(ctx, workload, v1alpha1.PhaseRunning, conditionDuplicateTarget); err != nil {
		return ctrl.Result{}, err
	}
	if released {
		r.announceHandBack(workload, replicas, c.Message)
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
		r.endWouldBePauseNow(workload)
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

// conditionChanged reports whether a condition says something else after a
// change than before it, including being set where there was none.
func conditionChanged(before, after *metav1.Condition) bool {
	if before == nil || after == nil {
		return before != after
	}
	return before.Status != after.Status || before.Reason != after.Reason || before.Message != after.Message
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
		Message:            truncated(message, maxConditionMessage),
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

// Field indexes on ManagedWorkloads, so a watch event finds the few a change
// concerns without listing, and copying, every one in the cluster.
const (
	// targetField is the Deployment or StatefulSet a ManagedWorkload
	// manages, as kind/name, in its own namespace.
	targetField = "hybernate.io/target"

	// dependencyField is each workload a ManagedWorkload depends on, as
	// namespace/kind/name: those dependsOn lists, and those it learned.
	dependencyField = "hybernate.io/dependency"
)

func targetKey(kind v1alpha1.TargetKind, name string) string {
	return string(kind) + "/" + name
}

func dependencyKey(id workloadID) string {
	return id.namespace + "/" + string(id.kind) + "/" + id.name
}

// workloadIndexes are the field indexes the watches look ManagedWorkloads
// up by.
func workloadIndexes() map[string]client.IndexerFunc {
	return map[string]client.IndexerFunc{
		targetField: func(obj client.Object) []string {
			w, ok := obj.(*v1alpha1.ManagedWorkload)
			if !ok {
				return nil
			}
			return []string{targetKey(w.Spec.Target.Kind, w.Spec.Target.Name)}
		},
		dependencyField: func(obj client.Object) []string {
			w, ok := obj.(*v1alpha1.ManagedWorkload)
			if !ok {
				return nil
			}
			refs := dependencyRefs(w)
			keys := make([]string, 0, len(refs))
			for _, ref := range refs {
				keys = append(keys, dependencyKey(dependencyID(w, ref)))
			}
			return keys
		},
	}
}

// workloadsBy enqueues the ManagedWorkloads a lookup by index finds, other
// than the one whose UID is skip, if it's set. They're only read for their
// names, so the cache's own objects are listed rather than copies.
func (r *Reconciler) workloadsBy(ctx context.Context, skip types.UID, opts ...client.ListOption) []reconcile.Request {
	var list v1alpha1.ManagedWorkloadList
	if err := r.List(ctx, &list, append(opts, client.UnsafeDisableDeepCopy)...); err != nil {
		log.FromContext(ctx).Error(err, "listing related managed workloads")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		if w := &list.Items[i]; skip == "" || w.UID != skip {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(w)})
		}
	}
	return requests
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
	requests := r.workloadsBy(ctx, changed.UID, client.InNamespace(changed.Namespace),
		client.MatchingFields{targetField: targetKey(changed.Spec.Target.Kind, changed.Spec.Target.Name)})
	for _, ref := range dependencyRefs(changed) {
		id := dependencyID(changed, ref)
		requests = append(requests, r.workloadsBy(ctx, changed.UID, client.InNamespace(id.namespace),
			client.MatchingFields{targetField: targetKey(id.kind, id.name)})...)
	}
	return append(requests, r.workloadsBy(ctx, changed.UID,
		client.MatchingFields{dependencyField: dependencyKey(targetID(changed))})...)
}

// relatedHandler enqueues what findRelatedWorkloads finds for a
// ManagedWorkload both before and after a change, so a dependency it no
// longer has re-checks the hold it took part in.
func (r *Reconciler) relatedHandler() handler.EventHandler {
	enqueue := func(q workqueue.TypedRateLimitingInterface[reconcile.Request], requests []reconcile.Request) {
		for _, req := range requests {
			q.Add(req)
		}
	}
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(q, r.findRelatedWorkloads(ctx, e.Object))
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(q, r.findRelatedWorkloads(ctx, e.ObjectOld))
			enqueue(q, r.findRelatedWorkloads(ctx, e.ObjectNew))
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(q, r.findRelatedWorkloads(ctx, e.Object))
		},
	}
}

// relationsChanged passes a ManagedWorkload's updates that its related
// workloads act on: its phase, which holds its dependencies awake and lets
// its dependents' resumes go on, its spec, its dependencies, and its
// deletion. Its own status writes otherwise, such as cost and activity
// flushes, concern no other workload.
func relationsChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			old, okOld := e.ObjectOld.(*v1alpha1.ManagedWorkload)
			cur, okNew := e.ObjectNew.(*v1alpha1.ManagedWorkload)
			if !okOld || !okNew {
				return true
			}
			return old.Status.Phase != cur.Status.Phase || old.Generation != cur.Generation ||
				!old.DeletionTimestamp.Equal(cur.DeletionTimestamp) ||
				!equality.Semantic.DeepEqual(old.Status.LearnedDependencies, cur.Status.LearnedDependencies)
		},
	}
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
	requests := r.workloadsBy(ctx, "", client.InNamespace(id.namespace),
		client.MatchingFields{targetField: targetKey(id.kind, id.name)})
	return append(requests, r.workloadsBy(ctx, "", client.MatchingFields{dependencyField: dependencyKey(id)})...)
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
	r.phaseWrites.record(workload.UID, now.Time)
	metrics.WorkloadPhase.DeleteLabelValues(workload.Namespace, workload.Name, string(old))
	recordPhase(workload)
	metrics.LifecycleTransitions.WithLabelValues(string(old), string(phase)).Inc()
	logger.Info("phase transition", "from", old, "to", phase, "reason", reason)
	return nil
}

// phaseWriteLag is how long after this operator writes a workload's phase
// its cache is taken to possibly still hold the one before. A watch event
// normally arrives within a second; one later than this means the
// operator's connection to the API server is in trouble.
const phaseWriteLag = 30 * time.Second

// phaseWrites remembers when this operator last wrote each workload's
// phase, which is the only time its cache can show a stale phase: a newly
// elected leader starts from a fresh list.
type phaseWrites struct {
	mu sync.Mutex
	at map[types.UID]time.Time
}

func (w *phaseWrites) record(uid types.UID, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.at == nil {
		w.at = make(map[types.UID]time.Time)
	}
	w.at[uid] = at
}

// recent reports whether the workload's phase was written within
// phaseWriteLag of now, forgetting a write that's older.
func (w *phaseWrites) recent(uid types.UID, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	at, ok := w.at[uid]
	if !ok {
		return false
	}
	if now.Sub(at) > phaseWriteLag {
		delete(w.at, uid)
		return false
	}
	return true
}

func (w *phaseWrites) forget(uid types.UID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.at, uid)
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
	if r.prometheus == nil {
		r.prometheus = signal.NewPrometheus(r.PrometheusURL)
	}
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.initDefaults()
	if err := r.setupDoormanSliceSync(mgr); err != nil {
		return err
	}
	for field, index := range workloadIndexes() {
		if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.ManagedWorkload{}, field, index); err != nil {
			return fmt.Errorf("indexing managed workloads by %s: %w", field, err)
		}
	}
	targetHandler := handler.EnqueueRequestsFromMapFunc(r.findWorkloadsForTarget)
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ManagedWorkload{}).
		Watches(&v1alpha1.ManagedWorkload{}, wakeHandler()).
		Watches(&v1alpha1.ManagedWorkload{}, r.relatedHandler(), builder.WithPredicates(relationsChanged())).
		Watches(&v1alpha1.ManagedWorkload{}, handler.EnqueueRequestsFromMapFunc(r.findRoutedNeighbours),
			builder.WithPredicates(phaseChanged())).
		Watches(&appsv1.Deployment{}, targetHandler).
		Watches(&appsv1.StatefulSet{}, targetHandler).
		Watches(&appsv1.Deployment{}, r.scaleUpHandler()).
		Watches(&appsv1.StatefulSet{}, r.scaleUpHandler()).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(r.findWorkloadsForDoorman),
			builder.WithPredicates(predicate.NewPredicateFuncs(r.isDoormanEndpoints))).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.findPausedWorkloadsInNamespace)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.findWorkloadsInNamespace),
			builder.WithPredicates(inWatchedNamespaces(r.WatchNamespaces))).
		Named("managedworkload").
		WithOptions(controller.Options{MaxConcurrentReconciles: max(r.MaxConcurrentReconciles, 1),
			UsePriorityQueue: ptr.To(true)}).
		Complete(r)
}
