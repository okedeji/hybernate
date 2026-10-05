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

// interfaces (defined at point of consumption) ---

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

// engineRegistry holds each workload's forecast engine between reconciles.
// The engine in memory is the authority while the operator runs; the state
// it exports to the workload's status is where a restarted operator, or a
// new leader, picks up. Engines are keyed by UID, so a workload deleted and
// created again under the same name starts afresh.
type engineRegistry struct {
	mu        sync.Mutex
	engines   map[types.UID]forecaster
	newEngine func() forecaster
	restore   func(state string) (forecaster, error)
}

func newEngineRegistry(newEngine func() forecaster) *engineRegistry {
	return &engineRegistry{
		engines:   make(map[types.UID]forecaster),
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
	reg.mu.Unlock()
}

// --- Reconcile automation ---

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

	// Feed engine hourly — prediction learns regardless of desiredState.
	if now := r.now(); r.metrics != nil && !engine.Observed(now) {
		metric, err := r.observedCPU(ctx, workload)
		if err != nil {
			return r.reportMetricsUnavailable(ctx, workload, err)
		}
		r.setCondition(workload, conditionMetricsAvailable, metav1.ConditionTrue, "MetricsReported", "")
		r.observeHour(ctx, workload, engine, metric, now)
	}

	// Always update prediction status so the user sees progress.
	r.updatePredictionStatus(ctx, workload, engine)

	// If manual desiredState is set, prediction still learns but
	// automation does not act. Status is updated above.
	if workload.Spec.DesiredState != nil {
		opmetrics.AutomationSkipped.WithLabelValues(workload.Namespace, workload.Name).Inc()
		r.emitEvent(workload, false, "Normal", ReasonAutomationSkipped, actionEvaluate,
			"automation skipped, desiredState is manually set to %s", *workload.Spec.DesiredState)
		return &ctrl.Result{RequeueAfter: 1 * time.Hour}, nil
	}

	if workload.Spec.IdlePolicy == nil {
		return &ctrl.Result{RequeueAfter: 1 * time.Hour}, nil
	}

	logger.V(1).Info("automation tick", "workload", workload.Name, "namespace", workload.Namespace,
		"engine_phase", engine.GetPhase(), "data_points", engine.GetDataPoints())
	return r.reconcileIdleClock(ctx, workload, target, engine)
}

// reconcileWake resumes a paused workload when an activity annotation asks
// for it, or ahead of the demand a confident forecast predicts.
func (r *Reconciler) reconcileWake(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) (*ctrl.Result, error) {
	if workload.Spec.DesiredState != nil {
		return nil, nil
	}

	if r.wokenByActivity(ctx, workload, target) {
		message := "activity annotation is newer than the pause, waking"
		if wakeSource(workload) == v1alpha1.ActivitySourceRequest {
			message = "a request is waiting, waking"
		}
		r.emitEvent(workload, workload.Spec.DryRun, "Normal", ReasonWokeByActivity, actionResume, "%s", message)
		if workload.Spec.DryRun {
			return nil, nil
		}
		return r.handleResume(ctx, workload)
	}

	engine := r.forecastEngine(workload)
	r.observePausedHour(ctx, workload, engine)
	r.updatePredictionStatus(ctx, workload, engine)

	if workload.Spec.IdlePolicy == nil || !workload.Spec.IdlePolicy.AutoResume {
		return nil, nil
	}
	if engine.GetPhase() < forecast.DailyActive {
		return nil, nil
	}

	// The workload is scaled to zero, so live requests can't be read. The
	// snapshot taken at pause time converts predicted millicores to a share
	// of what the workload requests across all its replicas.
	var totalRequest float64
	if workload.Status.Pause != nil && workload.Status.Pause.Resources != nil {
		totalRequest = float64(workload.Status.Pause.Resources.CPUMillis) * float64(workload.Status.Pause.PreviousReplicas)
	}
	if totalRequest <= 0 {
		return nil, nil
	}
	now := r.now()
	predicted, when := engine.Predict(0, now), "this hour"
	if untilNextHour(now) <= autoResumeLead {
		if next := engine.Predict(1, now); next > predicted {
			predicted, when = next, "next hour"
		}
	}
	predictedPercent := predicted / totalRequest * 100
	threshold := cpuThresholdFor(workload)
	if predictedPercent < float64(threshold) {
		return nil, nil
	}

	r.emitEvent(workload, workload.Spec.DryRun, "Normal", ReasonAutoResume, actionResume,
		"forecast expects %.0f%% utilization %s (threshold %d%%), waking ahead of demand", predictedPercent, when, threshold)
	if workload.Spec.DryRun {
		return nil, nil
	}
	return r.handleResume(ctx, workload)
}

// autoResumeLead is how far ahead of a predicted busy hour a paused workload
// wakes, so it's Ready when people arrive rather than starting as they do.
const autoResumeLead = 15 * time.Minute

func untilNextHour(now time.Time) time.Duration {
	return now.Truncate(time.Hour).Add(time.Hour).Sub(now)
}

// observePausedHour feeds the forecast an hour of no demand while the
// workload is paused behind the doorman: any request would have woken it,
// so an hour paused is an hour nobody asked for it. Without the doorman,
// demand while paused can't be seen, and nothing is recorded.
func (r *Reconciler) observePausedHour(ctx context.Context, workload *v1alpha1.ManagedWorkload, engine forecaster) {
	if !meta.IsStatusConditionTrue(workload.Status.Conditions, conditionWakeOnRequest) {
		return
	}
	if now := r.now(); !engine.Observed(now) {
		r.observeHour(ctx, workload, engine, 0, now)
	}
}

// observeHour feeds the forecast the demand seen this hour. A rejected
// observation is logged and the hour tried again on the next reconcile:
// it means a bad metric, which mustn't hold up the workload's automation.
func (r *Reconciler) observeHour(ctx context.Context, workload *v1alpha1.ManagedWorkload, engine forecaster, demand float64, now time.Time) {
	prediction, err := engine.Observe(demand, now)
	if err != nil {
		log.FromContext(ctx).Error(err, "feeding the forecast",
			"workload", workload.Name, "namespace", workload.Namespace, "cpu_millis", demand)
		return
	}
	r.emitEvent(workload, false, "Normal", ReasonPredictionFed, actionForecast,
		"fed %.0fm CPU, forecast %.0fm, phase %s", demand, prediction, engine.GetPhase())

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
