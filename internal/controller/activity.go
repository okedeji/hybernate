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
	"strconv"
	"strings"
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

	// unobservedGap is the most a gap between evaluations may last before
	// the clock restarts, since the operator wasn't watching and can't claim
	// the workload was idle in the meantime. See unobservedAfter.
	unobservedGap = 2*activityCheckInterval + statusFlushInterval

	defaultIdleAfter    = 1 * time.Hour
	defaultCPUThreshold = 10

	conditionIdleVetoed = "IdleVetoed"

	conditionPrometheusQueryEmpty = "PrometheusQueryEmpty"
	ReasonPrometheusQueryEmpty    = "PrometheusQueryEmpty"

	ReasonInvalidActivityAnnotation = "InvalidActivityAnnotation"
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
func (r *Reconciler) observeActivity(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object, load cpuLoad) activityObservation {
	now := r.now()
	if workload.Status.Activity == nil {
		r.resetActivity(workload, v1alpha1.ActivitySourceCreated)
	}
	status := workload.Status.Activity
	remembered, inMemory := r.activityMemo.get(workload.UID)
	if inMemory {
		recordActivity(status, remembered.LastActivityTime.Time, remembered.LastActivitySource)
		if remembered.LastEvaluatedTime != nil &&
			(status.LastEvaluatedTime == nil || remembered.LastEvaluatedTime.After(status.LastEvaluatedTime.Time)) {
			status.LastEvaluatedTime = remembered.LastEvaluatedTime.DeepCopy()
		}
	}

	if last := status.LastEvaluatedTime; last != nil && now.Sub(last.Time) > unobservedAfter(workload, inMemory) {
		status.LastActivityTime = metav1.NewTime(now)
		status.LastActivitySource = v1alpha1.ActivitySourceUnobserved
	}

	if target != nil {
		if hash := r.templateHashes.of(workload.UID, target); hash != "" {
			if status.TemplateHash != "" && status.TemplateHash != hash {
				recordActivity(status, now, v1alpha1.ActivitySourceRollout)
			}
			status.TemplateHash = hash
		}
	}

	r.warnMalformedActivity(workload, target)
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

	if load.err != nil {
		obs.cpuErr = load.err
	} else if load.percent > float64(cpuThresholdFor(workload)) {
		recordActivity(status, now, v1alpha1.ActivitySourceCPU)
	}

	// With CPU unreadable nothing is decided this check, so Prometheus
	// isn't asked.
	if obs.cpuErr == nil {
		active, err := r.prometheusActive(ctx, workload)
		if err != nil {
			obs.prometheusErr = err
		} else if active {
			recordActivity(status, now, v1alpha1.ActivitySourcePrometheus)
		}
	}

	// A check where a source couldn't be read isn't an evaluation: the time
	// since the last one keeps growing, and once it's longer than
	// unobservedAfter the clock restarts as unobserved. Otherwise a source
	// back from an outage that answers "nothing now" would find the whole
	// outage idle and pause at once. A short blind spot, such as the minute
	// before a woken workload's new pods have metrics, leaves what woke it
	// as its last activity.
	if obs.cpuErr == nil && obs.prometheusErr == nil {
		evaluated := metav1.NewTime(now)
		status.LastEvaluatedTime = &evaluated
	}
	pauseAt := metav1.NewTime(status.LastActivityTime.Add(idleAfterFor(workload)))
	status.PauseAt = &pauseAt
	r.activityMemo.put(workload.UID, *status.DeepCopy())
	recordIdleSeconds(workload, now)
	return obs
}

// unobservedAfter is how long since the last evaluation the operator is
// taken not to have been watching. Counted as idle, a gap could bring a
// pause forward by its whole length, so a short idleAfter tolerates a gap
// of at most half of it, though never less than two missed checks, which a
// busy operator's queue can delay. With no clock in memory, as after a
// restart or a change of leader, what's written can be a flush interval
// behind the last check, so the gap's start is unknown: anything beyond two
// checks restarts the clock, which can delay a pause but never bring one
// forward.
func unobservedAfter(workload *v1alpha1.ManagedWorkload, inMemory bool) time.Duration {
	if !inMemory {
		return 2 * activityCheckInterval
	}
	return min(unobservedGap, max(2*activityCheckInterval, idleAfterFor(workload)/2))
}

// recordIdleSeconds publishes how long the workload has gone without
// activity. A paused workload's clock stands still at its last activity, so
// the time keeps growing while it's paused, as the gauge says it does.
func recordIdleSeconds(workload *v1alpha1.ManagedWorkload, now time.Time) {
	a := workload.Status.Activity
	if a == nil {
		return
	}
	opmetrics.IdleSeconds.WithLabelValues(workload.Namespace, workload.Name).
		Set(now.Sub(a.LastActivityTime.Time).Seconds())
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

// wellFormedActivityTime reports whether an activity annotation's value is
// an RFC 3339 time, the only form that counts as activity.
func wellFormedActivityTime(raw string) bool {
	_, err := time.Parse(time.RFC3339, raw)
	return err == nil
}

// annotationWarnings remembers, for each workload, the malformed activity
// annotation values it has been warned about, so each is reported once
// rather than on every check.
type annotationWarnings struct {
	mu     sync.Mutex
	warned map[types.UID]map[string]string
}

// first reports whether value is new for key, recording it.
func (w *annotationWarnings) first(uid types.UID, key, value string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.warned == nil {
		w.warned = make(map[types.UID]map[string]string)
	}
	if w.warned[uid] == nil {
		w.warned[uid] = make(map[string]string)
	}
	if last, ok := w.warned[uid][key]; ok && last == value {
		return false
	}
	w.warned[uid][key] = value
	return true
}

func (w *annotationWarnings) clear(uid types.UID, key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.warned[uid], key)
}

func (w *annotationWarnings) forget(uid types.UID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.warned, uid)
}

// warnMalformedActivity reports each activity annotation on the workload or
// its target that isn't an RFC 3339 time, once per value. Such a value is
// ignored everywhere: it neither keeps an awake workload awake nor wakes a
// paused one. A tool stamping epoch seconds, say, would otherwise wake the
// workload only to see it pause again an idleAfter later, with nothing to
// say why.
func (r *Reconciler) warnMalformedActivity(workload *v1alpha1.ManagedWorkload, target client.Object) {
	sources := []struct {
		what string
		obj  client.Object
	}{
		{what: "ManagedWorkload " + workload.Name, obj: workload},
		{what: string(workload.Spec.Target.Kind) + " " + workload.Spec.Target.Name, obj: target},
	}
	for _, s := range sources {
		if s.obj == nil {
			continue
		}
		for _, key := range activityAnnotations {
			id := s.what + "/" + key
			raw, ok := s.obj.GetAnnotations()[key]
			if !ok || raw == "" || wellFormedActivityTime(raw) {
				r.activityWarned.clear(workload.UID, id)
				continue
			}
			if r.activityWarned.first(workload.UID, id, raw) {
				r.emitEvent(workload, false, "Warning", ReasonInvalidActivityAnnotation, actionEvaluateIdle,
					"%s on %s is %q, not an RFC 3339 time such as %s, so it's ignored: it neither keeps the workload "+
						"awake nor wakes it", key, s.what, raw, r.now().UTC().Format(time.RFC3339))
			}
		}
	}
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
// annotations was set to a time other than the one recorded.
func activityAnnotationChanged(obj client.Object, recorded map[string]string) bool {
	for _, key := range activityAnnotations {
		if v := obj.GetAnnotations()[key]; v != "" && v != recorded[key] && wellFormedActivityTime(v) {
			return true
		}
	}
	return false
}

// prometheusBudget bounds all of one workload's Prometheus queries in a
// check together, so a workload with many slow queries can't hold a
// reconcile worker for a timeout each.
const prometheusBudget = 5 * time.Second

// prometheusActive reports whether any configured Prometheus activity query
// returns a value above zero.
func (r *Reconciler) prometheusActive(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error) {
	p := workload.Spec.IdlePolicy
	if p == nil || p.Activity == nil || len(p.Activity.Prometheus) == 0 {
		return false, nil
	}
	if r.prometheus == nil {
		return false, signal.ErrEndpointNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, prometheusBudget)
	defer cancel()
	for _, q := range p.Activity.Prometheus {
		res, err := r.prometheus.Check(ctx, q.PromQL)
		if err != nil {
			return false, fmt.Errorf("evaluating %q: %w", q.PromQL, err)
		}
		r.querySeries.saw(workload.UID, q.PromQL, !res.NoSeries)
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

// querySeries remembers, for each workload's Prometheus activity queries,
// whether each has matched any series since the operator started or the
// query was written. A query that never has most likely names a metric or
// label that doesn't exist, and would never keep the workload awake. A
// query not yet evaluated, because one before it showed activity, is
// neither.
type querySeries struct {
	mu     sync.Mutex
	series map[types.UID]map[string]bool
}

func (s *querySeries) saw(uid types.UID, query string, matched bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.series == nil {
		s.series = make(map[types.UID]map[string]bool)
	}
	if s.series[uid] == nil {
		s.series[uid] = make(map[string]bool)
	}
	s.series[uid][query] = s.series[uid][query] || matched
}

// neverMatched is which of queries have been evaluated and have never
// matched a series.
func (s *querySeries) neverMatched(uid types.UID, queries []v1alpha1.PrometheusActivity) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var empty []string
	for _, q := range queries {
		if matched, evaluated := s.series[uid][q.PromQL]; evaluated && !matched {
			empty = append(empty, q.PromQL)
		}
	}
	return empty
}

func (s *querySeries) forget(uid types.UID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.series, uid)
}

// reportEmptyQueries points out activity queries that have never matched a
// series. It's informational: the other sources still decide, as an empty
// result from a query that's right says no one is using the workload. The
// event marks the condition beginning.
func (r *Reconciler) reportEmptyQueries(workload *v1alpha1.ManagedWorkload) {
	var empty []string
	if hasPrometheusActivity(workload) {
		empty = r.querySeries.neverMatched(workload.UID, workload.Spec.IdlePolicy.Activity.Prometheus)
	}
	if len(empty) == 0 {
		r.clearCondition(workload, conditionPrometheusQueryEmpty, "QueriesMatchSeries")
		return
	}
	began := !meta.IsStatusConditionTrue(workload.Status.Conditions, conditionPrometheusQueryEmpty)
	msg := fmt.Sprintf("Prometheus activity queries have matched no series since they were first evaluated: %s; "+
		"check their metric and label names in Prometheus, since until a query matches, it can't keep the workload awake",
		strings.Join(quoted(empty), ", "))
	r.setCondition(workload, conditionPrometheusQueryEmpty, metav1.ConditionTrue, "NoSeries", msg)
	if began {
		r.emitEvent(workload, false, "Warning", ReasonPrometheusQueryEmpty, actionEvaluateIdle, "%s", msg)
	}
}

func quoted(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = strconv.Quote(v)
	}
	return out
}

// templateHashes keeps each workload's target's pod template hash, for the
// version of the target it was worked out from. The clock and learning
// dependencies both need it on every check, and working it out encodes the
// whole template.
type templateHashes struct {
	mu     sync.Mutex
	hashes map[types.UID]templateHash
}

type templateHash struct {
	target          types.UID
	resourceVersion string
	hash            string
}

// of is the hash of target, the workload's target. A target never written
// to the API server, with no resource version, is hashed every time.
func (h *templateHashes) of(workload types.UID, target client.Object) string {
	version := templateHash{target: target.GetUID(), resourceVersion: target.GetResourceVersion()}
	if version.resourceVersion == "" {
		return podTemplateHash(target)
	}
	h.mu.Lock()
	cached, ok := h.hashes[workload]
	h.mu.Unlock()
	if ok && cached.target == version.target && cached.resourceVersion == version.resourceVersion {
		return cached.hash
	}
	version.hash = podTemplateHash(target)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hashes == nil {
		h.hashes = make(map[types.UID]templateHash)
	}
	h.hashes[workload] = version
	return version.hash
}

func (h *templateHashes) forget(workload types.UID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.hashes, workload)
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
	engine forecaster, load cpuLoad) (*ctrl.Result, error) {
	now := r.now()
	vetoed := false
	defer func() {
		if !vetoed {
			r.clearIdleVeto(workload)
		}
	}()
	obs := r.observeActivity(ctx, workload, target, load)
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
	r.reportEmptyQueries(workload)

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
		if vetoed, predicted, hour = r.forecastVeto(workload, engine); vetoed {
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
// before it still within idleAfter. An activity annotation is stamped to
// the second, so one in the second the clock ran out counts: it can't be
// told from one just before, and counting it only keeps the workload awake.
func activeSinceClockRanOut(workload *v1alpha1.ManagedWorkload, now, pauseAt time.Time) bool {
	if began := workload.Status.LastTransitionTime; workload.Status.Phase == v1alpha1.PhaseIdle && began != nil {
		activity := workload.Status.Activity
		if activity.LastActivitySource == v1alpha1.ActivitySourceAnnotation {
			return !activity.LastActivityTime.Time.Before(began.Truncate(time.Second))
		}
		return activity.LastActivityTime.After(began.Time)
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
func (r *Reconciler) forecastVeto(workload *v1alpha1.ManagedWorkload, engine forecaster) (vetoed bool, predicted float64, hour time.Time) {
	if engine == nil || engine.GetPhase() < forecast.DailyActive {
		return false, 0, time.Time{}
	}
	now := r.now()
	hour = r.hourStart(now)
	predicted = engine.Predict(0, now)
	if next := engine.Predict(1, now); next > predicted {
		predicted, hour = next, hour.Add(time.Hour)
	}
	return predicted >= float64(cpuThresholdFor(workload)), predicted, hour
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
// it.
func (r *Reconciler) wokenByActivity(workload *v1alpha1.ManagedWorkload, target client.Object) bool {
	r.warnMalformedActivity(workload, target)
	var recorded v1alpha1.WakeAnnotations
	if pause := workload.Status.Pause; pause != nil && pause.WakeAnnotations != nil {
		recorded = *pause.WakeAnnotations
	}
	return activityAnnotationChanged(workload, recorded.Workload) ||
		target != nil && activityAnnotationChanged(target, recorded.Target)
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
