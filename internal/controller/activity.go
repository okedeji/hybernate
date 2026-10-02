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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
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

func resolveIdleAction(workload *v1alpha1.ManagedWorkload) v1alpha1.IdleAction {
	if p := workload.Spec.IdlePolicy; p != nil && p.Action == v1alpha1.IdleActionDestroy {
		return v1alpha1.IdleActionDestroy
	}
	return v1alpha1.IdleActionPause
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
func (r *Reconciler) observeActivity(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) activityObservation {
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

	active, err := r.cpuActive(ctx, workload)
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
	if lastActivity.After(now) {
		lastActivity = now
	}
	return lastActivity, parse(v1alpha1.AnnotationActiveUntil)
}

// cpuActive reports whether CPU usage is above the activity threshold, as a
// percentage of the CPU requested across all replicas.
func (r *Reconciler) cpuActive(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error) {
	usage, err := r.observedCPU(ctx, workload)
	if err != nil {
		return false, err
	}
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
		res, err := signal.NewPrometheus(r.prometheusURL, q.PromQL).Check(ctx, workload.Namespace, workload.Name)
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
	var template corev1.PodTemplateSpec
	switch t := target.(type) {
	case *appsv1.Deployment:
		template = t.Spec.Template
	case *appsv1.StatefulSet:
		template = t.Spec.Template
	default:
		return ""
	}
	data, err := json.Marshal(template)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// reconcileIdleClock pauses or destroys a workload once it has been inactive
// for IdleAfter. Phase Idle means the clock has run out: in dry-run the
// workload stays Idle and reports what it would do; otherwise the idle action
// runs immediately.
func (r *Reconciler) reconcileIdleClock(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object, engine forecaster) (*ctrl.Result, error) {
	now := r.now()
	obs := r.observeActivity(ctx, workload, target)
	pauseAt := workload.Status.Activity.PauseAt.Time

	if obs.cpuErr != nil {
		return r.reportMetricsUnavailable(ctx, workload, obs.cpuErr)
	}
	r.setCondition(workload, conditionMetricsAvailable, metav1.ConditionTrue, "MetricsReported", "")

	if obs.prometheusErr != nil {
		return r.reportPrometheusUnavailable(ctx, workload, obs.prometheusErr)
	}
	if hasPrometheusActivity(workload) {
		r.setCondition(workload, conditionPrometheusAvailable, metav1.ConditionTrue, "QueriesEvaluated", "")
	}

	if obs.activeUntil.After(now) || now.Before(pauseAt) {
		if workload.Status.Phase == v1alpha1.PhaseIdle {
			r.emitEvent(workload, workload.Spec.DryRun, "Normal", ReasonActivityResumed, actionEvaluateIdle,
				"activity resumed (%s), no longer idle", workload.Status.Activity.LastActivitySource)
			if _, err := r.transition(ctx, workload, v1alpha1.PhaseRunning, "ActivityResumed"); err != nil {
				return nil, err
			}
		}
		return &ctrl.Result{RequeueAfter: nextCheck(now, pauseAt, obs.activeUntil)}, nil
	}

	if vetoed, predicted := r.forecastVeto(ctx, workload, engine); vetoed {
		r.emitEvent(workload, workload.Spec.DryRun, "Normal", ReasonIdleVetoed, actionEvaluateIdle,
			"idle, but the forecast expects demand within the hour (%.0f%% of requests); not pausing yet", predicted)
		return &ctrl.Result{RequeueAfter: activityCheckInterval}, nil
	}

	if held, err := r.dependencyHold(ctx, workload); held != nil || err != nil {
		return held, err
	}

	action := resolveIdleAction(workload)
	idleFor := now.Sub(workload.Status.Activity.LastActivityTime.Time).Round(time.Minute)

	if workload.Status.Phase != v1alpha1.PhaseIdle {
		opmetrics.IdleDetections.WithLabelValues(string(action), workload.Namespace, workload.Name).Inc()
		if workload.Spec.DryRun {
			opmetrics.DryrunActions.WithLabelValues("idle_" + string(action)).Inc()
		}
		r.emitEvent(workload, workload.Spec.DryRun, "Normal", ReasonIdleDetected, actionEvaluateIdle,
			"no activity for %s, last seen from %s; %s", idleFor, workload.Status.Activity.LastActivitySource, action)
		if _, err := r.transition(ctx, workload, v1alpha1.PhaseIdle, "IdleDetected"); err != nil {
			return nil, err
		}
	}

	if workload.Spec.DryRun {
		return &ctrl.Result{RequeueAfter: activityCheckInterval}, nil
	}
	if action == v1alpha1.IdleActionDestroy {
		return r.handleDestroy(ctx, workload)
	}
	return r.handlePause(ctx, workload)
}

// forecastVeto defers a pause when a confident forecast expects demand in
// the next hour above the activity threshold.
func (r *Reconciler) forecastVeto(ctx context.Context, workload *v1alpha1.ManagedWorkload, engine forecaster) (bool, float64) {
	if engine == nil || engine.GetPhase() < forecast.DailyActive {
		return false, 0
	}
	perReplica, err := r.metrics.CPURequestPerReplica(ctx, workload)
	if err != nil {
		return false, 0
	}
	replicas, err := r.metrics.Replicas(ctx, workload)
	if err != nil || replicas == 0 {
		return false, 0
	}
	predicted := engine.Predict(1, r.now()) / (perReplica * float64(replicas)) * 100
	return predicted >= float64(cpuThresholdFor(workload)), predicted
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
// to wake: a last-activity newer than the pause, or an active-until hold that
// hasn't ended.
func (r *Reconciler) wokenByActivity(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) bool {
	now := r.now()
	var pausedAt time.Time
	if workload.Status.Pause != nil && workload.Status.Pause.PausedAt != nil {
		pausedAt = workload.Status.Pause.PausedAt.Time
	}
	for _, obj := range []client.Object{workload, target} {
		if obj == nil {
			continue
		}
		lastActivity, activeUntil := r.activityAnnotations(ctx, obj, now)
		if lastActivity.After(pausedAt) || activeUntil.After(now) {
			return true
		}
	}
	return false
}

// reportPrometheusUnavailable surfaces a Prometheus activity source that can't
// be evaluated. The clock can't see that activity, so it doesn't act.
func (r *Reconciler) reportPrometheusUnavailable(ctx context.Context, workload *v1alpha1.ManagedWorkload, err error) (*ctrl.Result, error) {
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
	return &ctrl.Result{RequeueAfter: activityCheckInterval}, nil
}
