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

// metricsReader measures a workload two ways. Activity compares the usage
// of the workload's own containers with their requests, so an injected
// sidecar's background work doesn't keep it awake. Cost counts the whole
// pod's requests, sidecars included, since that's what the workload
// reserves and what pausing frees.
type metricsReader interface {
	WorkloadCPUMillis(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
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
	newEngine func() forecaster
	restore   func(state string) (forecaster, error)
}

func newEngineRegistry(newEngine func() forecaster) *engineRegistry {
	return &engineRegistry{
		engines:   make(map[types.UID]forecaster),
		hours:     make(map[types.UID]demandHour),
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
	reg.mu.Unlock()
}

// demandHour is what has been seen of a workload's demand in one hour of
// the forecast's clock.
type demandHour struct {
	start time.Time
	// peak is the most CPU, in millicores, any one look found.
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
	var usage float64
	if r.metrics != nil {
		var err error
		if usage, err = r.observedCPU(ctx, workload); err != nil {
			r.clearIdleVeto(workload)
			return r.reportMetricsUnavailable(ctx, workload, err)
		}
		r.setCondition(workload, conditionMetricsAvailable, metav1.ConditionTrue, "MetricsReported", "")
		r.seeDemand(ctx, workload, engine, r.awakeDemand(workload, usage), true)
	}
	r.updatePredictionStatus(ctx, workload, engine)

	if workload.Spec.DesiredState != nil {
		r.clearIdleVeto(workload)
		if err := r.reportManualOverride(ctx, workload); err != nil {
			return nil, err
		}
		return &ctrl.Result{RequeueAfter: activityCheckInterval}, nil
	}
	r.clearCondition(workload, conditionManualOverride, "Automated")

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
	return r.reconcileIdleClock(ctx, workload, target, engine, usage)
}

// reportManualOverride notes, once, that desiredState has taken over from
// automation, which goes on learning the forecast but doesn't act on it.
func (r *Reconciler) reportManualOverride(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	desired := *workload.Spec.DesiredState
	msg := fmt.Sprintf("desiredState is %s, so automation doesn't pause or wake the workload", desired)
	if c := meta.FindStatusCondition(workload.Status.Conditions, conditionManualOverride); c != nil &&
		c.Status == metav1.ConditionTrue && c.Message == msg {
		return nil
	}
	r.setCondition(workload, conditionManualOverride, metav1.ConditionTrue, "DesiredStateSet", msg)
	if err := r.Status().Update(ctx, workload); err != nil {
		return fmt.Errorf("recording the manual override: %w", err)
	}
	opmetrics.AutomationSkipped.WithLabelValues(workload.Namespace, workload.Name).Inc()
	r.emitEvent(workload, false, "Normal", ReasonAutomationSkipped, actionEvaluate,
		"automation skipped, desiredState is manually set to %s", desired)
	return nil
}

// reconcileWake resumes a paused workload when an activity annotation asks
// for it, or ahead of the demand a confident forecast predicts. Otherwise
// it's looked at again when that forecast could next change its mind.
func (r *Reconciler) reconcileWake(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) (*ctrl.Result, error) {
	recheck := &ctrl.Result{RequeueAfter: r.pausedRecheck()}
	r.clearIdleVeto(workload)
	engine := r.forecastEngine(workload)
	manual := workload.Spec.DesiredState != nil
	woken := !manual && r.wokenByActivity(ctx, workload, target)
	r.seePausedDemand(ctx, workload, engine, manual, woken)
	r.updatePredictionStatus(ctx, workload, engine)

	if manual {
		return recheck, r.reportManualOverride(ctx, workload)
	}
	r.clearCondition(workload, conditionManualOverride, "Automated")

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

	totalRequest := pausedRequest(workload)
	if totalRequest <= 0 {
		return recheck, nil
	}
	now := r.now()
	predicted, when := engine.Predict(0, now), "this hour"
	if r.untilNextHour(now) <= autoResumeLead {
		if next := engine.Predict(1, now); next > predicted {
			predicted, when = next, "next hour"
		}
	}
	predictedPercent := predicted / totalRequest * 100
	threshold := cpuThresholdFor(workload)
	if predictedPercent < float64(threshold) {
		return recheck, nil
	}

	return r.handleResume(ctx, workload, &phaseEvent{reason: ReasonAutoResume, message: fmt.Sprintf(
		"forecast expects %.0f%% utilization %s (threshold %d%%), waking ahead of demand", predictedPercent, when, threshold)})
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

// pausedRequest is the CPU the workload requested across its replicas
// before it paused, from the snapshot taken then: scaled to zero, it has no
// pods to read requests from.
func pausedRequest(workload *v1alpha1.ManagedWorkload) float64 {
	pause := workload.Status.Pause
	if pause == nil || pause.Resources == nil {
		return 0
	}
	return float64(pause.Resources.CPUMillis) * float64(pause.PreviousReplicas)
}

// seePausedDemand is a look at a paused workload's demand. Behind the
// doorman, any request would have woken it, so a look that finds it still
// paused found no demand, and one that finds a request waking it found
// some. Without the doorman, or under a manual pause that requests don't
// end, demand can't be seen at all.
//
// The demand a wake shows is at least twice the activity threshold, of
// what the workload requested: the forecast approaches a pattern it is
// learning from below, so an hour seen at the threshold itself would never
// quite be forecast to reach it. Once awake, the workload's own usage
// counts if it's more.
func (r *Reconciler) seePausedDemand(ctx context.Context, workload *v1alpha1.ManagedWorkload, engine forecaster, manual, woken bool) {
	routed := meta.IsStatusConditionTrue(workload.Status.Conditions, conditionWakeOnRequest)
	var demand float64
	if woken {
		demand = 2 * pausedRequest(workload) * float64(cpuThresholdFor(workload)) / 100
	}
	r.seeDemand(ctx, workload, engine, demand, (routed || woken) && !manual)
}

// resumeWarmup is how long after a wake an awake workload's usage isn't
// counted as demand. An autoResume wake lands at most autoResumeLead before
// the hour it's for, so that long covers what such a wake brings about.
const resumeWarmup = autoResumeLead

// awakeDemand is the demand an awake workload's usage shows. Starting up,
// filling caches and compiling hot paths isn't anyone asking for it.
// Counted, an autoResume wake would teach the forecast that the hour it
// woke in is busy, and a week later it would wake an hour earlier, and
// earlier again the week after. A wake by request shows its demand anyway.
func (r *Reconciler) awakeDemand(workload *v1alpha1.ManagedWorkload, usage float64) float64 {
	if woke := workload.Status.LastActedAt; woke != nil && r.now().Sub(woke.Time) < resumeWarmup {
		return 0
	}
	return usage
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
			"workload", workload.Name, "namespace", workload.Namespace, "cpu_millis", hour.peak)
		return
	}
	r.emitEvent(workload, false, "Normal", ReasonPredictionFed, actionForecast,
		"the hour from %s peaked at %.0fm CPU, forecast %.0fm, phase %s",
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
func (r *Reconciler) observedCPU(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error) {
	millis, err := r.metrics.WorkloadCPUMillis(ctx, workload)
	if !errors.Is(err, opmetrics.ErrNoPodMetrics) {
		return millis, err
	}
	if replicas, rerr := r.metrics.Replicas(ctx, workload); rerr == nil && replicas == 0 {
		return 0, nil
	}
	return 0, err
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
		msg = "target sets no CPU requests, so CPU activity can't be measured and idle detection is paused; set CPU requests on its containers"
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
