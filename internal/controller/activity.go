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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
	"github.com/okedeji/hybernate/internal/forecast"
	opmetrics "github.com/okedeji/hybernate/internal/metrics"
	"github.com/okedeji/hybernate/internal/signal"
)

const (
	// activityCheckInterval is how often polled sources (CPU) are read while
	// a workload is awake.
	activityCheckInterval = 1 * time.Minute

	// unobservedGap is how stale the last evaluation may be before the clock
	// restarts. Evaluations are only written every statusFlushInterval, so a
	// gap beyond that plus two missed checks means the operator wasn't
	// watching, and it can't claim the workload was idle in the meantime.
	unobservedGap = 2*activityCheckInterval + statusFlushInterval

	defaultIdleAfter    = 1 * time.Hour
	defaultCPUThreshold = 10

	conditionIdleVetoed = "IdleVetoed"
)

// activityMemo keeps each workload's latest clock between status writes.
// Status is written every statusFlushInterval at most when only the clock
// has moved, so without this, activity seen on an unwritten check would be
// lost and the workload could pause up to one flush interval early. It's
// in-memory only: after a restart the written status is the baseline.
type activityMemo struct {
	mu     sync.Mutex
	clocks map[types.UID]v1alpha1.ActivityStatus
}

func (m *activityMemo) get(uid types.UID) (v1alpha1.ActivityStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	clock, ok := m.clocks[uid]
	return clock, ok
}

func (m *activityMemo) put(uid types.UID, clock v1alpha1.ActivityStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.clocks == nil {
		m.clocks = make(map[types.UID]v1alpha1.ActivityStatus)
	}
	m.clocks[uid] = clock
}

func (m *activityMemo) forget(uid types.UID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.clocks, uid)
}

// errNoCPURequests means CPU utilization can't be computed for the target.
var errNoCPURequests = errors.New("target has no CPU requests")

// activityObservation is what one evaluation of the clock learned beyond the
// timestamps it records in status.
type activityObservation struct {
	// activeUntil is a hold from the active-until annotation; zero if none.
	activeUntil time.Time
	// cpuErr is set when CPU utilization couldn't be read. Without it the
	// operator can't tell a busy workload from an idle one, so it won't act.
	cpuErr error
	// prometheusErr is set when a configured Prometheus query couldn't be
	// evaluated, which blinds the clock to that source in the same way.
	prometheusErr error
}

func idleAfterFor(workload *v1alpha1.ManagedWorkload) time.Duration {
	if p := workload.Spec.IdlePolicy; p != nil && p.IdleAfter != nil && p.IdleAfter.Duration > 0 {
		return p.IdleAfter.Duration
	}
	return defaultIdleAfter
}

func cpuThresholdFor(workload *v1alpha1.ManagedWorkload) int {
	if p := workload.Spec.IdlePolicy; p != nil && p.Activity != nil && p.Activity.CPUThreshold > 0 {
		return p.Activity.CPUThreshold
	}
	return defaultCPUThreshold
}

// recordActivity moves the clock forward when t is newer than the last
// recorded activity. Older activity never moves it back.
func recordActivity(status *v1alpha1.ActivityStatus, t time.Time, source v1alpha1.ActivitySource) {
	if t.After(status.LastActivityTime.Time) {
		status.LastActivityTime = metav1.NewTime(t)
		status.LastActivitySource = source
	}
}

// resetActivity starts the clock fresh, as on creation or wake.
func (r *Reconciler) resetActivity(workload *v1alpha1.ManagedWorkload, source v1alpha1.ActivitySource) {
	now := r.clockTime()
	hash := ""
	if workload.Status.Activity != nil {
		hash = workload.Status.Activity.TemplateHash
	}
	pauseAt := metav1.NewTime(now.Add(idleAfterFor(workload)))
	workload.Status.Activity = &v1alpha1.ActivityStatus{
		LastActivityTime:   now,
		LastActivitySource: source,
		PauseAt:            &pauseAt,
		LastEvaluatedTime:  &now,
		TemplateHash:       hash,
	}
}

// observeActivity reads every activity source and advances the clock in
// workload.Status.Activity. Any single source is enough to keep the workload
// awake, so sources are combined by taking the latest time.
func (r *Reconciler) observeActivity(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object, usage float64) activityObservation {
	now := r.now()
	if workload.Status.Activity == nil {
		r.resetActivity(workload, v1alpha1.ActivitySourceCreated)
	}
	status := workload.Status.Activity
	if remembered, ok := r.activityMemo.get(workload.UID); ok {
		recordActivity(status, remembered.LastActivityTime.Time, remembered.LastActivitySource)
		if remembered.LastEvaluatedTime != nil &&
			(status.LastEvaluatedTime == nil || remembered.LastEvaluatedTime.After(status.LastEvaluatedTime.Time)) {
			status.LastEvaluatedTime = remembered.LastEvaluatedTime.DeepCopy()
		}
	}

	if last := status.LastEvaluatedTime; last != nil && now.Sub(last.Time) > unobservedGap {
		status.LastActivityTime = metav1.NewTime(now)
		status.LastActivitySource = v1alpha1.ActivitySourceUnobserved
	}

	if target != nil {
		if hash := podTemplateHash(target); hash != "" {
			if status.TemplateHash != "" && status.TemplateHash != hash {
				recordActivity(status, now, v1alpha1.ActivitySourceRollout)
			}
			status.TemplateHash = hash
		}
	}

	var obs activityObservation
	for _, obj := range []client.Object{workload, target} {
		if obj == nil {
			continue
		}
		lastActivity, activeUntil := r.activityAnnotations(ctx, obj, now)
		if !lastActivity.IsZero() {
			recordActivity(status, lastActivity, v1alpha1.ActivitySourceAnnotation)
		}
		if activeUntil.After(obs.activeUntil) {
			obs.activeUntil = activeUntil
		}
	}

	active, err := r.cpuActive(ctx, workload, usage)
	if err != nil {
		obs.cpuErr = err
	} else if active {
		recordActivity(status, now, v1alpha1.ActivitySourceCPU)
	}

	active, err = r.prometheusActive(ctx, workload)
	if err != nil {
		obs.prometheusErr = err
	} else if active {
		recordActivity(status, now, v1alpha1.ActivitySourcePrometheus)
	}

	evaluated := metav1.NewTime(now)
	status.LastEvaluatedTime = &evaluated
	pauseAt := metav1.NewTime(status.LastActivityTime.Add(idleAfterFor(workload)))
	status.PauseAt = &pauseAt
	r.activityMemo.put(workload.UID, *status.DeepCopy())
	opmetrics.IdleSeconds.WithLabelValues(workload.Namespace, workload.Name).
		Set(now.Sub(status.LastActivityTime.Time).Seconds())
	return obs
}

// activityAnnotations parses the activity annotations on obj. A last-activity
// time in the future is treated as now: a deliberate hold uses active-until,
// which states when it ends.
func (r *Reconciler) activityAnnotations(ctx context.Context, obj client.Object, now time.Time) (lastActivity, activeUntil time.Time) {
	annotations := obj.GetAnnotations()
	parse := func(key string) time.Time {
		raw, ok := annotations[key]
		if !ok {
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			log.FromContext(ctx).Info("ignoring malformed activity annotation",
				"object", obj.GetName(), "namespace", obj.GetNamespace(), "annotation", key, "value", raw)
			return time.Time{}
		}
		return t
	}

	lastActivity = parse(v1alpha1.AnnotationLastActivity)
	if lastRequest := parse(v1alpha1.AnnotationLastRequest); lastRequest.After(lastActivity) {
		lastActivity = lastRequest
	}
	if lastActivity.After(now) {
		lastActivity = now
	}
	return lastActivity, parse(v1alpha1.AnnotationActiveUntil)
}

// wakeSource is what to record as the activity that woke a paused workload:
// a request the doorman stamped since the pause began, or any other wake. It
// must be read before the resume completes, which clears the pause.
func wakeSource(workload *v1alpha1.ManagedWorkload) v1alpha1.ActivitySource {
	pause := workload.Status.Pause
	if pause == nil {
		return v1alpha1.ActivitySourceWoke
	}
	raw, ok := workload.Annotations[v1alpha1.AnnotationLastRequest]
	if !ok {
		return v1alpha1.ActivitySourceWoke
	}
	if pause.WakeAnnotations != nil && raw != pause.WakeAnnotations.Workload[v1alpha1.AnnotationLastRequest] {
		return v1alpha1.ActivitySourceRequest
	}
	requested, err := time.Parse(time.RFC3339, raw)
	if err != nil || pause.PausedAt == nil || requested.Before(pause.PausedAt.Time) {
		return v1alpha1.ActivitySourceWoke
	}
	return v1alpha1.ActivitySourceRequest
}

// activityAnnotations are the annotations that wake a paused workload.
var activityAnnotations = []string{
	v1alpha1.AnnotationLastActivity,
	v1alpha1.AnnotationLastRequest,
	v1alpha1.AnnotationActiveUntil,
}

// activityAnnotationValues is what obj's activity annotations are set to.
func activityAnnotationValues(obj client.Object) map[string]string {
	if obj == nil {
		return nil
	}
	var values map[string]string
	for _, key := range activityAnnotations {
		if v, ok := obj.GetAnnotations()[key]; ok {
			if values == nil {
				values = map[string]string{}
			}
			values[key] = v
		}
	}
	return values
}

// activityAnnotationChanged reports whether any of obj's activity
// annotations was set to a value other than the one recorded.
func activityAnnotationChanged(obj client.Object, recorded map[string]string) bool {
	for _, key := range activityAnnotations {
		if v := obj.GetAnnotations()[key]; v != "" && v != recorded[key] {
			return true
		}
	}
	return false
}

// cpuActive reports whether CPU usage is above the activity threshold, as a
// percentage of the CPU requested across all replicas.
func (r *Reconciler) cpuActive(ctx context.Context, workload *v1alpha1.ManagedWorkload, usage float64) (bool, error) {
	if usage == 0 {
		return false, nil
	}
	perReplica, err := r.metrics.CPURequestPerReplica(ctx, workload)
	if err != nil {
		return false, fmt.Errorf("%w: %w", errNoCPURequests, err)
	}
	replicas, err := r.metrics.Replicas(ctx, workload)
	if err != nil {
		return false, fmt.Errorf("reading replicas: %w", err)
	}
	requested := perReplica * float64(replicas)
	if requested <= 0 {
		return false, errNoCPURequests
	}
	return usage/requested*100 > float64(cpuThresholdFor(workload)), nil
}

// prometheusActive reports whether any configured Prometheus activity query
// returns a value above zero.
func (r *Reconciler) prometheusActive(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error) {
	p := workload.Spec.IdlePolicy
	if p == nil || p.Activity == nil {
		return false, nil
	}
	for _, q := range p.Activity.Prometheus {
		res, err := signal.NewPrometheus(r.prometheusURL, q.PromQL).Check(ctx)
		if err != nil {
			return false, fmt.Errorf("evaluating %q: %w", q.PromQL, err)
		}
		if res.Confirm {
			return true, nil
		}
	}
	return false, nil
}

func hasPrometheusActivity(workload *v1alpha1.ManagedWorkload) bool {
	p := workload.Spec.IdlePolicy
	return p != nil && p.Activity != nil && len(p.Activity.Prometheus) > 0
}

// podTemplateHash fingerprints the target's pod template. Scaling changes
// spec.replicas and bumps metadata.generation, so the generation can't tell
// a deploy from Hybernate's own pause and resume; the template can.
func podTemplateHash(target client.Object) string {
	switch target.(type) {
	case *appsv1.Deployment, *appsv1.StatefulSet:
	default:
		return ""
	}
	data, err := json.Marshal(podTemplateOf(target))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// reconcileIdleClock pauses a workload once it has been inactive for
// IdleAfter. Phase Idle means the clock has run out, or a pause was
// requested: in dry-run the workload stays Idle and reports the pause it
// would make; otherwise it pauses.
func (r *Reconciler) reconcileIdleClock(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object,
	engine forecaster, usage float64) (*ctrl.Result, error) {
	now := r.now()
	vetoed := false
	defer func() {
		if !vetoed {
			r.clearIdleVeto(workload)
		}
	}()
	obs := r.observeActivity(ctx, workload, target, usage)
	pauseAt := workload.Status.Activity.PauseAt.Time

	if obs.cpuErr != nil {
		return r.reportMetricsUnavailable(ctx, workload, obs.cpuErr)
	}
	r.setCondition(workload, conditionMetricsAvailable, metav1.ConditionTrue, "MetricsReported", "")

	if obs.prometheusErr != nil {
		return r.reportPrometheusUnavailable(ctx, workload, obs.prometheusErr), nil
	}
	if hasPrometheusActivity(workload) {
		r.setCondition(workload, conditionPrometheusAvailable, metav1.ConditionTrue, "QueriesEvaluated", "")
	}

	if obs.activeUntil.After(now) || activeSinceClockRanOut(workload, now, pauseAt) {
		if workload.Status.Phase == v1alpha1.PhaseIdle {
			source := workload.Status.Activity.LastActivitySource
			slept, freed, measured := r.endWouldBePause(workload)
			if err := r.transition(ctx, workload, v1alpha1.PhaseRunning, "ActivityResumed"); err != nil {
				return nil, err
			}
			r.reportActivityResumed(workload, source, slept, freed, measured)
		}
		return &ctrl.Result{RequeueAfter: nextCheck(now, pauseAt, obs.activeUntil)}, nil
	}

	// The forecast only holds back a pause not yet decided. Idle has decided
	// it, and in dry-run measures it as under way, as a real one would be.
	if workload.Status.Phase == v1alpha1.PhaseRunning {
		var predicted float64
		var hour time.Time
		if vetoed, predicted, hour = r.forecastVeto(ctx, workload, engine); vetoed {
			return r.reportIdleVetoed(ctx, workload, predicted, hour)
		}
	}

	if held, err := r.dependencyHold(ctx, workload); held != nil || err != nil {
		return held, err
	}

	idleFor := now.Sub(workload.Status.Activity.LastActivityTime.Time).Round(time.Minute)

	if workload.Status.Phase != v1alpha1.PhaseIdle {
		if workload.Spec.DryRun {
			r.beginWouldBePause(ctx, workload)
		}
		if err := r.transition(ctx, workload, v1alpha1.PhaseIdle, "IdleDetected"); err != nil {
			return nil, err
		}
		opmetrics.IdleDetections.WithLabelValues(workload.Namespace, workload.Name).Inc()
		if workload.Spec.DryRun {
			opmetrics.DryrunActions.WithLabelValues("idle_pause").Inc()
		}
		r.emitEvent(workload, workload.Spec.DryRun, "Normal", ReasonIdleDetected, actionEvaluateIdle,
			"no activity for %s, last seen from %s; pause", idleFor, workload.Status.Activity.LastActivitySource)
	}

	if workload.Spec.DryRun {
		return &ctrl.Result{RequeueAfter: activityCheckInterval}, nil
	}
	if until, held := gitOpsHold(workload, now); held {
		return &ctrl.Result{RequeueAfter: until.Sub(now)}, nil
	}
	return r.handlePause(ctx, workload, target, nil)
}

// activeSinceClockRanOut reports whether there has been activity since the
// workload's clock last ran out. Running, it's within idleAfter. Idle, the
// pause it's in, or would be in under dry-run, began at the move to Idle,
// and only activity after that ends it, as only that wakes a paused
// workload: a pause request runs the clock out early, with the activity seen
// before it still within idleAfter.
func activeSinceClockRanOut(workload *v1alpha1.ManagedWorkload, now, pauseAt time.Time) bool {
	if began := workload.Status.LastTransitionTime; workload.Status.Phase == v1alpha1.PhaseIdle && began != nil {
		return workload.Status.Activity.LastActivityTime.After(began.Time)
	}
	return now.Before(pauseAt)
}

func (r *Reconciler) reportActivityResumed(workload *v1alpha1.ManagedWorkload, source v1alpha1.ActivitySource,
	slept time.Duration, freed float64, measured bool) {
	if !measured {
		r.emitEvent(workload, workload.Spec.DryRun, "Normal", ReasonActivityResumed, actionEvaluateIdle,
			"activity resumed (%s), no longer idle", source)
		return
	}
	r.emitEvent(workload, true, "Normal", ReasonActivityResumed, actionEvaluateIdle,
		"activity resumed (%s): would have slept %s, freeing %s; %s",
		source, roundedDuration(slept), cost.FormatDollars(freed), dryRunSummary(workload.Status.DryRun))
}

// reportIdleVetoed holds back a pause because the forecast expects demand
// soon. The condition says so for as long as the veto lasts. The event
// marks it beginning, and is sent once the condition is written, so a
// failed write doesn't announce it twice.
func (r *Reconciler) reportIdleVetoed(ctx context.Context, workload *v1alpha1.ManagedWorkload, predicted float64, hour time.Time) (*ctrl.Result, error) {
	began := !meta.IsStatusConditionTrue(workload.Status.Conditions, conditionIdleVetoed)
	msg := fmt.Sprintf("idle, but the forecast expects demand at %.0f%% of requests in the hour from %s; not pausing yet",
		predicted, hour.UTC().Format("15:04 UTC"))
	r.setCondition(workload, conditionIdleVetoed, metav1.ConditionTrue, "ForecastExpectsDemand", msg)
	if began {
		if err := r.Status().Update(ctx, workload); err != nil {
			return nil, fmt.Errorf("recording the forecast's veto: %w", err)
		}
		r.emitEvent(workload, workload.Spec.DryRun, "Normal", ReasonIdleVetoed, actionEvaluateIdle, "%s", msg)
	}
	return &ctrl.Result{RequeueAfter: activityCheckInterval}, nil
}

func (r *Reconciler) clearIdleVeto(workload *v1alpha1.ManagedWorkload) {
	r.clearCondition(workload, conditionIdleVetoed, "NotVetoed")
}

// forecastVeto defers a pause while a confident forecast expects demand
// above the activity threshold in the hour under way or the next. It
// covers every hour autoResume would wake the workload for, so a workload
// is never paused only to be woken straight back. It reports the busiest
// of the two hours, and when it begins.
func (r *Reconciler) forecastVeto(ctx context.Context, workload *v1alpha1.ManagedWorkload, engine forecaster) (vetoed bool, predicted float64, hour time.Time) {
	if engine == nil || engine.GetPhase() < forecast.DailyActive {
		return false, 0, time.Time{}
	}
	requested, err := r.requestedCPU(ctx, workload)
	if err != nil {
		log.FromContext(ctx).V(1).Info("not consulting the forecast before pausing: the CPU requested can't be read",
			"workload", workload.Name, "namespace", workload.Namespace, "error", err.Error())
		return false, 0, time.Time{}
	}
	if requested <= 0 {
		return false, 0, time.Time{}
	}
	now := r.now()
	hour = r.hourStart(now)
	predicted = engine.Predict(0, now) / requested * 100
	if next := engine.Predict(1, now) / requested * 100; next > predicted {
		predicted, hour = next, hour.Add(time.Hour)
	}
	return predicted >= float64(cpuThresholdFor(workload)), predicted, hour
}

// requestedCPU is the CPU the workload requests across its replicas.
func (r *Reconciler) requestedCPU(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error) {
	perReplica, err := r.metrics.CPURequestPerReplica(ctx, workload)
	if err != nil {
		return 0, fmt.Errorf("reading CPU requests: %w", err)
	}
	replicas, err := r.metrics.Replicas(ctx, workload)
	if err != nil {
		return 0, fmt.Errorf("reading replicas: %w", err)
	}
	return perReplica * float64(replicas), nil
}

// nextCheck is when the clock next needs evaluating: at the next CPU poll,
// or sooner if the pause deadline or a hold ends first.
func nextCheck(now, pauseAt, activeUntil time.Time) time.Duration {
	wait := activityCheckInterval
	for _, deadline := range []time.Time{pauseAt, activeUntil} {
		if d := deadline.Sub(now); d > 0 && d < wait {
			wait = d
		}
	}
	return wait
}

// wokenByActivity reports whether a paused workload's annotations ask for it
// to wake. Annotations are stamped to the second, and from clocks other than
// the operator's, such as a laptop's, so a change since the pause began
// counts whatever time it states, and only a change counts: a last-activity
// set before a requested pause, which ran the clock out early, doesn't undo
// it. A pause recorded without the annotations, by an older version, wakes
// on a last-activity no older than the pause, or an active-until hold that
// hasn't ended.
func (r *Reconciler) wokenByActivity(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) bool {
	now := r.now()
	var pausedAt time.Time
	var recorded *v1alpha1.WakeAnnotations
	if pause := workload.Status.Pause; pause != nil {
		recorded = pause.WakeAnnotations
		if pause.PausedAt != nil {
			pausedAt = pause.PausedAt.Time
		}
	}
	if recorded != nil {
		return activityAnnotationChanged(workload, recorded.Workload) ||
			target != nil && activityAnnotationChanged(target, recorded.Target)
	}
	for _, obj := range []client.Object{workload, target} {
		if obj == nil {
			continue
		}
		lastActivity, activeUntil := r.activityAnnotations(ctx, obj, now)
		if (!lastActivity.IsZero() && !lastActivity.Before(pausedAt)) || activeUntil.After(now) {
			return true
		}
	}
	return false
}

// reportPrometheusUnavailable surfaces a Prometheus activity source that can't
// be evaluated. The clock can't see that activity, so it doesn't act.
func (r *Reconciler) reportPrometheusUnavailable(ctx context.Context, workload *v1alpha1.ManagedWorkload, err error) *ctrl.Result {
	reason := "QueryFailed"
	msg := fmt.Sprintf("a Prometheus activity query failed, so idle detection is paused: %v", err)
	if errors.Is(err, signal.ErrEndpointNotConfigured) {
		reason = "EndpointNotConfigured"
		msg = "Prometheus activity queries are configured but the operator has no --prometheus-url, so idle detection is paused"
	}

	firstFailure := !meta.IsStatusConditionFalse(workload.Status.Conditions, conditionPrometheusAvailable)
	r.setCondition(workload, conditionPrometheusAvailable, metav1.ConditionFalse, reason, msg)
	if firstFailure {
		r.emitEvent(workload, false, "Warning", reason, actionEvaluateIdle, "%s", msg)
	}
	log.FromContext(ctx).V(1).Info("Prometheus activity unavailable",
		"workload", workload.Name, "namespace", workload.Namespace, "reason", reason, "error", err.Error())
	return &ctrl.Result{RequeueAfter: activityCheckInterval}
}
