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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
	"github.com/okedeji/hybernate/internal/forecast"
	opmetrics "github.com/okedeji/hybernate/internal/metrics"

	ctrl "sigs.k8s.io/controller-runtime"
)

type forecaster interface {
	Observe(actual float64, now time.Time) (float64, error)
	Observed(now time.Time) bool
	Predict(h int, now time.Time) float64
	Export() (string, error)
	Configure(settings forecast.Settings)
	GetPhase() forecast.Phase
	DailyConfidence() int
	WeeklyConfidence() int
	GetDataPoints() int
	RegimeChanged() bool
	AnomalyDetected() bool
}

// metricsReader measures a workload two ways. Activity, and the forecast,
// compare the usage of the workload's own containers with their requests,
// so an injected sidecar's background work doesn't keep it awake. Cost
// counts the whole pod's requests, sidecars included, since that's what the
// workload reserves and what pausing frees.
type metricsReader interface {
	WorkloadCPUMillis(ctx context.Context, workload *v1alpha1.ManagedWorkload) (cpuMillis float64, pods int, err error)
	CPURequestPerReplica(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
	PodRequestsPerReplica(ctx context.Context, workload *v1alpha1.ManagedWorkload) (cpuMillis, memBytes float64, err error)
	Replicas(ctx context.Context, workload *v1alpha1.ManagedWorkload) (int32, error)
	TotalPVCBytes(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
}

// listPricer prices a workload at the nodes it runs on.
type listPricer interface {
	ListRates(ctx context.Context, workload *v1alpha1.ManagedWorkload) (cost.Rates, bool, error)
}

// engineRegistry holds each workload's forecast engine between reconciles,
// and the hour of demand it is seeing. The engine in memory is the
// authority while the operator runs; the state it exports to the workload's
// status is where a restarted operator, or a new leader, picks up. The hour
// under way is in memory only. Engines are keyed by UID, so a workload
// deleted and created again under the same name starts afresh.
type engineRegistry struct {
	mu        sync.Mutex
	engines   map[types.UID]forecaster
	hours     map[types.UID]demandHour
	exports   map[types.UID]exportedState
	newEngine func() forecaster
	restore   func(state string) (forecaster, error)
}

// exportedState is the state an engine last exported, and the data points
// it had then. Only an observation changes what an engine has learned, and
// each adds a data point, so the state stands until the count moves.
type exportedState struct {
	engine     forecaster
	dataPoints int
	state      string
}

func newEngineRegistry(newEngine func() forecaster) *engineRegistry {
	return &engineRegistry{
		engines:   make(map[types.UID]forecaster),
		hours:     make(map[types.UID]demandHour),
		exports:   make(map[types.UID]exportedState),
		newEngine: newEngine,
		restore: func(state string) (forecaster, error) {
			e, err := forecast.ImportEngine(state, forecast.Settings{})
			if err != nil {
				return nil, err
			}
			return e, nil
		},
	}
}

// getOrCreate returns the workload's engine with settings applied, restoring
// it from state the first time. State that can't be restored is discarded
// for a new engine, and the error says why.
func (reg *engineRegistry) getOrCreate(uid types.UID, settings forecast.Settings, state string) (forecaster, error) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	e, ok := reg.engines[uid]
	var restoreErr error
	if !ok {
		e, restoreErr = reg.load(state)
		reg.engines[uid] = e
	}
	e.Configure(settings)
	return e, restoreErr
}

func (reg *engineRegistry) load(state string) (forecaster, error) {
	if state == "" {
		return reg.newEngine(), nil
	}
	e, err := reg.restore(state)
	if err != nil {
		return reg.newEngine(), fmt.Errorf("restoring forecast state: %w", err)
	}
	return e, nil
}

// forget drops a workload's engine, once the workload is gone.
func (reg *engineRegistry) forget(uid types.UID) {
	reg.mu.Lock()
	delete(reg.engines, uid)
	delete(reg.hours, uid)
	delete(reg.exports, uid)
	reg.mu.Unlock()
}

// export is what the workload's engine has learned, encoded for its status.
// Encoding compresses the whole state, so it's done again only once the
// engine has observed another hour.
func (reg *engineRegistry) export(uid types.UID, engine forecaster) (string, error) {
	points := engine.GetDataPoints()
	reg.mu.Lock()
	last, ok := reg.exports[uid]
	reg.mu.Unlock()
	if ok && last.engine == engine && last.dataPoints == points {
		return last.state, nil
	}
	state, err := engine.Export()
	if err != nil {
		return "", err
	}
	reg.mu.Lock()
	reg.exports[uid] = exportedState{engine: engine, dataPoints: points, state: state}
	reg.mu.Unlock()
	return state, nil
}

// demandHour is what has been seen of a workload's demand in one hour of
// the forecast's clock.
type demandHour struct {
	start time.Time
	// peak is the highest utilization, as a percentage of the CPU
	// requested, any one look found.
	peak        float64
	first, last time.Time
	// blind is set by a look that couldn't see demand: the workload was
	// paused where a request wouldn't wake it.
	blind bool
}

// whole reports whether the hour was seen from its start to its end, with
// no look that was blind to demand. An operator that started partway
// through the hour, or stopped looking before it ended, may have missed its
// busiest minutes, and the forecast would learn a busy hour as a quiet one.
// A gap within the hour isn't held against it: a slow wake or pause leaves
// one, and the looks either side of it show the demand around it.
func (h demandHour) whole() bool {
	return !h.blind && h.first.Sub(h.start) <= unobservedGap && h.start.Add(time.Hour).Sub(h.last) <= unobservedGap
}

// see adds a look at a workload's demand, made at now in the hour that
// began at start. When the look is the first since an hour ended, that
// hour is returned, if it was seen whole.
func (reg *engineRegistry) see(uid types.UID, start, now time.Time, demand float64, visible bool) (demandHour, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	prev, ok := reg.hours[uid]
	h := prev
	if !ok || !prev.start.Equal(start) {
		h = demandHour{start: start, first: now}
	}
	h.last = now
	h.peak = max(h.peak, demand)
	h.blind = h.blind || !visible
	reg.hours[uid] = h
	return prev, ok && prev.start.Before(start) && prev.whole()
}

func (r *Reconciler) reconcileAutomation(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) (*ctrl.Result, error) {
	phase := workload.Status.Phase
	if phase != v1alpha1.PhasePaused && phase != v1alpha1.PhaseRunning && phase != v1alpha1.PhaseIdle {
		return nil, nil
	}

	if err := r.checkDependenciesExist(ctx, workload); err != nil {
		return nil, err
	}
	if phase == v1alpha1.PhasePaused {
		return r.reconcileWake(ctx, workload, target)
	}

	logger := log.FromContext(ctx)
	engine := r.forecastEngine(workload)

	// The forecast learns whether or not automation acts on it, so every
	// awake workload is looked at as often as the activity clock looks,
	// for the forecast to see each hour's busiest minutes.
	var load cpuLoad
	if r.metrics != nil {
		usage, pods, err := r.observedCPU(ctx, workload)
		if err != nil {
			r.clearIdleVeto(workload)
			if workload.Spec.IdlePolicy != nil {
				r.observeActivity(ctx, workload, target, cpuLoad{err: err})
			}
			return r.reportMetricsUnavailable(ctx, workload, err)
		}
		load.percent, load.err = r.utilization(ctx, workload, usage, pods)
		if load.err == nil {
			r.setCondition(workload, conditionMetricsAvailable, metav1.ConditionTrue, "MetricsReported", "")
		}
		r.seeDemand(ctx, workload, engine, r.awakeDemand(workload, load.percent), load.err == nil)
	}
	r.updatePredictionStatus(ctx, workload, engine)

	// Under dry-run, Idle is a would-be pause being measured, which a pause
	// request begins whatever the idle policy, and only activity ends.
	measuring := workload.Spec.DryRun && phase == v1alpha1.PhaseIdle
	if workload.Spec.IdlePolicy == nil && !measuring {
		r.clearIdleVeto(workload)
		if phase == v1alpha1.PhaseIdle {
			if err := r.transition(ctx, workload, v1alpha1.PhaseRunning, "NoIdlePolicy"); err != nil {
				return nil, err
			}
		}
		return &ctrl.Result{RequeueAfter: activityCheckInterval}, nil
	}

	logger.V(1).Info("automation tick", "workload", workload.Name, "namespace", workload.Namespace,
		"engine_phase", engine.GetPhase(), "data_points", engine.GetDataPoints())
	return r.reconcileIdleClock(ctx, workload, target, engine, load)
}

// cpuLoad is what one look at an awake workload's CPU found: its
// utilization, or why it couldn't be worked out.
type cpuLoad struct {
	percent float64
	err     error
}

// utilization is CPU usage as a percentage of what the workload's own
// containers request in the pods it was measured over. Divided by every
// replica's request, the usage of one pod serving while the others are
// Pending or crash-looping would read as a fraction of itself, and an
// in-use workload as idle. The activity threshold, the
// forecast and every decision made on it share this one measure, so a
// forecast that holds an idle workload awake would wake it once paused.
// A target scaled to zero by something else is idle, though its pods may
// report usage for the seconds they take to terminate.
func (r *Reconciler) utilization(ctx context.Context, workload *v1alpha1.ManagedWorkload, usage float64, pods int) (float64, error) {
	if usage == 0 {
		return 0, nil
	}
	replicas, err := r.metrics.Replicas(ctx, workload)
	if err != nil {
		return 0, fmt.Errorf("reading replicas: %w", err)
	}
	if replicas == 0 {
		return 0, nil
	}
	perReplica, err := r.metrics.CPURequestPerReplica(ctx, workload)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errNoCPURequests, err)
	}
	requested := perReplica * float64(pods)
	if requested <= 0 {
		return 0, errNoCPURequests
	}
	return usage / requested * 100, nil
}

// reconcileWake resumes a paused workload when an activity annotation asks
// for it, or ahead of the demand a confident forecast predicts. Otherwise
// it's looked at again when that forecast could next change its mind.
func (r *Reconciler) reconcileWake(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) (*ctrl.Result, error) {
	recheck := &ctrl.Result{RequeueAfter: r.pausedRecheck()}
	r.clearIdleVeto(workload)
	recordIdleSeconds(workload, r.now())
	engine := r.forecastEngine(workload)
	woken := r.wokenByActivity(workload, target)
	r.seePausedDemand(ctx, workload, engine, woken)
	r.updatePredictionStatus(ctx, workload, engine)

	if woken {
		message := "activity annotation is newer than the pause, waking"
		if wakeSource(workload) == v1alpha1.ActivitySourceRequest {
			message = "a request is waiting, waking"
		}
		return r.handleResume(ctx, workload, &phaseEvent{reason: ReasonWokeByActivity, message: message})
	}

	if workload.Spec.IdlePolicy == nil || !workload.Spec.IdlePolicy.AutoResume {
		return recheck, nil
	}
	if engine.GetPhase() < forecast.DailyActive {
		return recheck, nil
	}

	now := r.now()
	predicted, when := engine.Predict(0, now), "this hour"
	if r.untilNextHour(now) <= autoResumeLead {
		if next := engine.Predict(1, now); next > predicted {
			predicted, when = next, "next hour"
		}
	}
	threshold := cpuThresholdFor(workload)
	if predicted < float64(threshold) {
		return recheck, nil
	}

	return r.handleResume(ctx, workload, &phaseEvent{reason: ReasonAutoResume, message: fmt.Sprintf(
		"forecast expects %.0f%% utilization %s (threshold %d%%), waking ahead of demand", predicted, when, threshold)})
}

// autoResumeLead is how far ahead of a predicted busy hour a paused workload
// wakes, so it's Ready when people arrive rather than starting as they do.
const autoResumeLead = 15 * time.Minute

// hourStart is when the forecast's hour containing t began. The forecast
// counts hours in r.Timezone, where an hour may begin at half or quarter
// past the hour in UTC. Stepping back to the local hour's start, rather
// than building it with time.Date, stays exact in the hour a clock change
// repeats.
func (r *Reconciler) hourStart(t time.Time) time.Time {
	loc := r.Timezone
	if loc == nil {
		loc = time.UTC
	}
	local := t.In(loc)
	into := time.Duration(local.Minute())*time.Minute +
		time.Duration(local.Second())*time.Second +
		time.Duration(local.Nanosecond())
	return t.Add(-into)
}

func (r *Reconciler) untilNextHour(now time.Time) time.Duration {
	return r.hourStart(now).Add(time.Hour).Sub(now)
}

// pausedRecheck is when a paused workload is next looked at: when autoResume
// could next decide differently, autoResumeLead before the hour and on it,
// and at least every statusFlushInterval, so the forecast learns each quiet
// hour and savings accrue steadily.
func (r *Reconciler) pausedRecheck() time.Duration {
	wait := statusFlushInterval
	next := r.untilNextHour(r.now())
	for _, d := range []time.Duration{next - autoResumeLead, next} {
		if d > 0 && d < wait {
			wait = d
		}
	}
	return wait
}

// seePausedDemand is a look at a paused workload's demand. Behind the
// doorman, any request would have woken it, so a look that finds it still
// paused found no demand, and one that finds a request waking it found
// some. Without the doorman, demand can't be seen at all.
//
// The demand a wake shows is twice the activity threshold: the forecast
// approaches a pattern it is learning from below, so an hour seen at the
// threshold itself would never quite be forecast to reach it. Once awake,
// the workload's own utilization counts if it's more.
func (r *Reconciler) seePausedDemand(ctx context.Context, workload *v1alpha1.ManagedWorkload, engine forecaster, woken bool) {
	routed := meta.IsStatusConditionTrue(workload.Status.Conditions, conditionWakeOnRequest)
	var demand float64
	if woken {
		demand = 2 * float64(cpuThresholdFor(workload))
	}
	r.seeDemand(ctx, workload, engine, demand, routed || woken)
}

// resumeWarmup is how long after a wake an awake workload's usage isn't
// counted as demand. An autoResume wake lands at most autoResumeLead before
// the hour it's for, so that long covers what such a wake brings about.
const resumeWarmup = autoResumeLead

// awakeDemand is the demand an awake workload's utilization shows. Starting up,
// filling caches and compiling hot paths isn't anyone asking for it.
// Counted, an autoResume wake would teach the forecast that the hour it
// woke in is busy, and a week later it would wake an hour earlier, and
// earlier again the week after. A wake by request shows its demand anyway.
//
// Once warmed up, a workload seen in use counts as at least twice the
// activity threshold, as a request that wakes it does. A light-use
// workload's use may show only in another activity source; taught its own
// light utilization on the days autoResume had it ready, and a request's
// twice the threshold on the days people woke it, the forecast would have
// it ready only every other morning.
func (r *Reconciler) awakeDemand(workload *v1alpha1.ManagedWorkload, percent float64) float64 {
	now := r.now()
	since := r.hourStart(now)
	if woke := workload.Status.LastActedAt; woke != nil {
		warm := woke.Add(resumeWarmup)
		if now.Before(warm) {
			return 0
		}
		if warm.After(since) {
			since = warm
		}
	}
	threshold := float64(cpuThresholdFor(workload))
	if percent > threshold || r.usedSince(workload, since) {
		return max(percent, 2*threshold)
	}
	return percent
}

// usedSince reports whether the activity clock last saw the workload in use
// at or after since. The clock's own restarts, on creation, a wake, a
// rollout or a gap in watching, aren't use.
func (r *Reconciler) usedSince(workload *v1alpha1.ManagedWorkload, since time.Time) bool {
	clock, ok := r.activityMemo.get(workload.UID)
	if !ok {
		if workload.Status.Activity == nil {
			return false
		}
		clock = *workload.Status.Activity
	}
	switch clock.LastActivitySource {
	case v1alpha1.ActivitySourceCPU, v1alpha1.ActivitySourceAnnotation, v1alpha1.ActivitySourcePrometheus:
		return !clock.LastActivityTime.Time.Before(since)
	}
	return false
}

// seeDemand adds a look at the workload's demand to the hour under way, and
// once an hour is over, feeds the forecast the peak seen in it. The peak,
// not the mean, is what the forecast's decisions need: the activity clock
// counts a workload as active for any minute above the threshold, so an
// hour busy for its last ten minutes is an hour it must be awake for.
func (r *Reconciler) seeDemand(ctx context.Context, workload *v1alpha1.ManagedWorkload, engine forecaster, demand float64, visible bool) {
	now := r.now()
	hour, ended := r.engines.see(workload.UID, r.hourStart(now), now, demand, visible)
	if !ended || engine.Observed(hour.start) {
		return
	}
	r.observeHour(ctx, workload, engine, hour)
}

// observeHour feeds the forecast an hour that has ended. A rejected
// observation means a bad metric: it's logged, and the hour is skipped
// rather than holding up the workload's automation.
func (r *Reconciler) observeHour(ctx context.Context, workload *v1alpha1.ManagedWorkload, engine forecaster, hour demandHour) {
	prediction, err := engine.Observe(hour.peak, hour.start)
	if err != nil {
		log.FromContext(ctx).Error(err, "feeding the forecast",
			"workload", workload.Name, "namespace", workload.Namespace, "cpu_percent", hour.peak)
		return
	}
	r.emitEvent(workload, false, "Normal", ReasonPredictionFed, actionForecast,
		"the hour from %s peaked at %.0f%% of the CPU requested, forecast %.0f%%, phase %s",
		hour.start.UTC().Format("15:04 UTC"), hour.peak, prediction, engine.GetPhase())

	if engine.RegimeChanged() {
		opmetrics.PredictionRegimeChanges.WithLabelValues(workload.Namespace, workload.Name).Inc()
		r.emitEvent(workload, false, "Warning", ReasonRegimeChange, actionForecast,
			"regime change detected, prediction engine demoted to %s", engine.GetPhase())
	}
	if engine.AnomalyDetected() {
		opmetrics.PredictionAnomalies.WithLabelValues(workload.Namespace, workload.Name).Inc()
	}
}

// observedCPU reads total CPU usage to feed the forecast. A target scaled to
// zero has no pods and therefore no pod metrics, but that is a real
// observation of zero demand, not missing data.
func (r *Reconciler) observedCPU(ctx context.Context, workload *v1alpha1.ManagedWorkload) (millis float64, pods int, err error) {
	millis, pods, err = r.metrics.WorkloadCPUMillis(ctx, workload)
	if !errors.Is(err, opmetrics.ErrNoPodMetrics) {
		return millis, pods, err
	}
	if replicas, rerr := r.metrics.Replicas(ctx, workload); rerr == nil && replicas == 0 {
		return 0, 0, nil
	}
	return 0, 0, err
}

// reportMetricsUnavailable surfaces why CPU can't be read. Both idle
// detection and the forecast depend on it, so without this the workload would
// sit awake indefinitely with nothing to say why.
func (r *Reconciler) reportMetricsUnavailable(ctx context.Context, workload *v1alpha1.ManagedWorkload, err error) (*ctrl.Result, error) {
	reason := "MetricsUnavailable"
	msg := fmt.Sprintf("cannot read CPU usage, so idle detection and the forecast are paused: %v", err)
	switch {
	case errors.Is(err, opmetrics.ErrNoPodMetrics):
		reason = "NoPodMetrics"
		msg = "target has replicas but no pod metrics, so idle detection and the forecast are paused; check that metrics-server is installed and reporting"
	case errors.Is(err, errNoCPURequests):
		reason = "NoCPURequests"
		msg = "the target's containers request no CPU, in its running pods or, with none running, in its pod template, " +
			"so CPU activity can't be measured and idle detection is paused; set CPU requests on its containers, " +
			"or a default in the namespace's LimitRange"
	}

	firstFailure := !meta.IsStatusConditionFalse(workload.Status.Conditions, conditionMetricsAvailable)
	r.setCondition(workload, conditionMetricsAvailable, metav1.ConditionFalse, reason, msg)
	if firstFailure {
		r.emitEvent(workload, false, "Warning", reason, actionForecast, "%s", msg)
	}
	log.FromContext(ctx).V(1).Info("CPU usage unavailable",
		"workload", workload.Name, "namespace", workload.Namespace, "reason", reason, "error", err.Error())

	return &ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
}
