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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/forecast"
	opmetrics "github.com/okedeji/hybernate/internal/metrics"
	"github.com/okedeji/hybernate/internal/policy"
	"github.com/okedeji/hybernate/internal/signal"

	ctrl "sigs.k8s.io/controller-runtime"
)

// interfaces (defined at point of consumption) ---

type forecaster interface {
	Observe(actual float64, now time.Time) float64
	Predict(h int, now time.Time) float64
	Export() ([]byte, error)
	GetPhase() forecast.Phase
	DailyConfidence() int
	WeeklyConfidence() int
	GetDataPoints() int
	RegimeChanged() bool
	AnomalyDetected() bool
}

type metricsReader interface {
	CPUUsage(ctx context.Context, workload *v1alpha1.ManagedWorkload) (resource.Quantity, error)
	MemoryUsage(ctx context.Context, workload *v1alpha1.ManagedWorkload) (resource.Quantity, error)
	TotalCPUMillis(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
	CPURequestPerReplica(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
	MemoryRequestPerReplica(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
	Replicas(ctx context.Context, workload *v1alpha1.ManagedWorkload) (int32, error)
	TotalMemoryBytes(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
	TotalPVCBytes(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
}

type idleEvaluator interface {
	Evaluate(ctx context.Context, namespace, name string, signals []signal.Checker, gracePeriod time.Duration) (policy.IdleEvaluation, error)
	StartGracePeriod(namespace, name string)
	Reset(namespace, name string)
}

// engineRegistry manages forecast engines per workload.
type engineRegistry struct {
	mu      sync.Mutex
	engines map[string]forecaster
	lastFed map[string]time.Time
	factory func(threshold int) forecaster
}

func newEngineRegistry(factory func(threshold int) forecaster) *engineRegistry {
	return &engineRegistry{
		engines: make(map[string]forecaster),
		lastFed: make(map[string]time.Time),
		factory: factory,
	}
}

func (reg *engineRegistry) getOrCreate(key string, threshold int, state *string) forecaster {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	if e, ok := reg.engines[key]; ok {
		return e
	}

	if state != nil {
		if restored, err := forecast.ImportEngine([]byte(*state)); err == nil {
			reg.engines[key] = restored
			return restored
		}
	}

	e := reg.factory(threshold)
	reg.engines[key] = e
	return e
}

func (reg *engineRegistry) shouldFeed(key string, now time.Time) bool {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	last, ok := reg.lastFed[key]
	if !ok {
		return true
	}
	return now.Sub(last) >= 1*time.Hour
}

func (reg *engineRegistry) markFed(key string, now time.Time) {
	reg.mu.Lock()
	reg.lastFed[key] = now
	reg.mu.Unlock()
}

// --- Reconcile automation ---

func (r *Reconciler) reconcileAutomation(ctx context.Context, workload *v1alpha1.ManagedWorkload) (*ctrl.Result, error) {
	phase := workload.Status.Phase

	// Paused workloads only participate in auto-resume checks.
	if phase == v1alpha1.PhasePaused {
		return r.reconcileAutoResume(ctx, workload)
	}

	if phase != v1alpha1.PhaseRunning && phase != v1alpha1.PhaseIdle {
		return nil, nil
	}

	logger := log.FromContext(ctx)
	key := workload.Namespace + "/" + workload.Name
	engine := r.engines.getOrCreate(key, workload.Spec.Prediction.Confidence, r.predictionState(ctx, workload))

	// Feed engine hourly — prediction learns regardless of desiredState.
	if r.metrics != nil && r.engines.shouldFeed(key, r.now()) {
		metric, err := r.observedCPU(ctx, workload)
		if err != nil {
			return r.reportMetricsUnavailable(ctx, workload, err)
		}
		r.setCondition(workload, conditionMetricsAvailable, metav1.ConditionTrue, "MetricsReported", "")
		prediction := engine.Observe(metric, r.now())
		r.engines.markFed(key, r.now())
		r.emitEvent(workload, false, "Normal", ReasonPredictionFed, actionForecast,
			"fed %.0fm CPU, forecast %.0fm, phase %s", metric, prediction, engine.GetPhase())

		if engine.RegimeChanged() {
			opmetrics.PredictionRegimeChanges.WithLabelValues(workload.Namespace, workload.Name).Inc()
			r.emitEvent(workload, false, "Warning", ReasonRegimeChange, actionForecast,
				"regime change detected, prediction engine demoted to %s", engine.GetPhase())
		}
		if engine.AnomalyDetected() {
			opmetrics.PredictionAnomalies.WithLabelValues(workload.Namespace, workload.Name).Inc()
		}
	}

	// Always update prediction status so the user sees progress.
	r.updatePredictionStatus(ctx, workload, engine)

	// If manual desiredState is set, prediction still learns but
	// automation does not act. Status is updated above.
	if workload.Spec.DesiredState != nil {
		opmetrics.AutomationSkipped.WithLabelValues(workload.Namespace, workload.Name).Inc()
		r.emitEvent(workload, false, "Normal", ReasonAutomationSkipped, actionEvaluate,
			"automation skipped, desiredState is manually set to %s", *workload.Spec.DesiredState)
		if err := r.Status().Update(ctx, workload); err != nil {
			return nil, fmt.Errorf("updating prediction status: %w", err)
		}
		result := ctrl.Result{RequeueAfter: 1 * time.Hour}
		return &result, nil
	}

	enginePhase := engine.GetPhase()
	logger.Info("automation tick", "engine_phase", enginePhase, "data_points", engine.GetDataPoints())

	switch enginePhase {
	case forecast.Observing:
		if err := r.Status().Update(ctx, workload); err != nil {
			return nil, fmt.Errorf("updating prediction status: %w", err)
		}
		result := ctrl.Result{RequeueAfter: 1 * time.Hour}
		return &result, nil

	case forecast.DailySuggesting:
		return r.reconcileAutomationPolicies(ctx, workload, engine, true)

	default: // DailyActive, WeeklySuggesting, FullyActive
		return r.reconcileAutomationPolicies(ctx, workload, engine, workload.Spec.DryRun)
	}
}

// observedCPU reads total CPU usage to feed the forecast. A target scaled to
// zero has no pods and therefore no pod metrics, but that is a real
// observation of zero demand, not missing data.
func (r *Reconciler) observedCPU(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error) {
	millis, err := r.metrics.TotalCPUMillis(ctx, workload)
	if !errors.Is(err, opmetrics.ErrNoPodMetrics) {
		return millis, err
	}
	if replicas, rerr := r.metrics.Replicas(ctx, workload); rerr == nil && replicas == 0 {
		return 0, nil
	}
	return 0, err
}

// reportMetricsUnavailable surfaces why the forecast can't be fed. Without
// it the workload shows Observing indefinitely, with nothing to say it has
// stopped learning.
func (r *Reconciler) reportMetricsUnavailable(ctx context.Context, workload *v1alpha1.ManagedWorkload, err error) (*ctrl.Result, error) {
	reason := "MetricsUnavailable"
	msg := fmt.Sprintf("cannot read CPU usage, forecast is not learning: %v", err)
	if errors.Is(err, opmetrics.ErrNoPodMetrics) {
		reason = "NoPodMetrics"
		msg = "target has replicas but no pod metrics, forecast is not learning; check that metrics-server is installed and reporting"
	}

	firstFailure := !meta.IsStatusConditionFalse(workload.Status.Conditions, conditionMetricsAvailable)
	r.setCondition(workload, conditionMetricsAvailable, metav1.ConditionFalse, reason, msg)
	if uerr := r.Status().Update(ctx, workload); uerr != nil {
		return nil, fmt.Errorf("updating metrics condition: %w", uerr)
	}
	if firstFailure {
		r.emitEvent(workload, false, "Warning", reason, actionForecast, "%s", msg)
	}
	log.FromContext(ctx).V(1).Info("forecast not fed",
		"workload", workload.Name, "namespace", workload.Namespace, "reason", reason, "error", err.Error())

	return &ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
}

func (r *Reconciler) reconcileAutoResume(ctx context.Context, workload *v1alpha1.ManagedWorkload) (*ctrl.Result, error) {
	if workload.Spec.IdlePolicy == nil || !workload.Spec.IdlePolicy.AutoResume {
		return nil, nil
	}

	// Manual desiredState takes precedence — don't fight the user.
	if workload.Spec.DesiredState != nil {
		return nil, nil
	}

	key := workload.Namespace + "/" + workload.Name
	engine := r.engines.getOrCreate(key, workload.Spec.Prediction.Confidence, r.predictionState(ctx, workload))

	enginePhase := engine.GetPhase()
	if enginePhase == forecast.Observing {
		return nil, nil
	}

	dryRun := enginePhase == forecast.DailySuggesting || workload.Spec.DryRun
	predicted := engine.Predict(0, r.now())
	cpuPercent := cpuIdlePercentFor(workload)

	// Use the paused resource snapshot to convert predicted millicores to
	// a percentage of total request — the workload is scaled to zero so we
	// cannot query live metrics. Predicted value is total across all replicas.
	var totalRequest float64
	if workload.Status.Pause != nil && workload.Status.Pause.Resources != nil {
		totalRequest = float64(workload.Status.Pause.Resources.CPUMillis) * float64(workload.Status.Pause.PreviousReplicas)
	}
	predictedPercent := 0.0
	if totalRequest > 0 {
		predictedPercent = predicted / totalRequest * 100
	}

	if predictedPercent < float64(cpuPercent) {
		return nil, nil
	}

	r.emitEvent(workload, dryRun, "Normal", ReasonAutoResume, actionResume,
		"prediction expects %.0f%% utilization (threshold %d%%), resuming", predictedPercent, cpuPercent)

	if dryRun {
		return nil, nil
	}

	r.idle.Reset(workload.Namespace, workload.Spec.Target.Name)
	return r.handleResume(ctx, workload)
}

func (r *Reconciler) reconcileAutomationPolicies(ctx context.Context, workload *v1alpha1.ManagedWorkload, engine forecaster, dryRun bool) (*ctrl.Result, error) {
	if workload.Spec.IdlePolicy != nil && r.metrics != nil {
		result, err := r.reconcileIdleAction(ctx, workload, engine, dryRun)
		if err != nil {
			return nil, err
		}
		if result != nil {
			return result, nil
		}
	}

	if err := r.Status().Update(ctx, workload); err != nil {
		return nil, fmt.Errorf("updating status: %w", err)
	}
	requeue := 1 * time.Minute
	if dryRun {
		requeue = 1 * time.Hour
	}
	result := ctrl.Result{RequeueAfter: requeue}
	return &result, nil
}

func (r *Reconciler) reconcileIdleAction(ctx context.Context, workload *v1alpha1.ManagedWorkload, engine forecaster, dryRun bool) (*ctrl.Result, error) {
	signals, err := r.buildIdleSignals(ctx, workload)
	if err != nil {
		return nil, fmt.Errorf("building idle signals: %w", err)
	}
	eval, err := r.idle.Evaluate(ctx, workload.Namespace, workload.Spec.Target.Name, signals, idleGracePeriod(workload))
	if err != nil {
		r.emitEvent(workload, dryRun, "Warning", ReasonIdleConsensus, actionEvaluateIdle,
			"failed to get idle signal consensus, %v", err)
		return nil, fmt.Errorf("evaluating idle: %w", err)
	}

	ns, name := workload.Namespace, workload.Name
	cpuPercent := cpuIdlePercentFor(workload)

	switch {
	case eval.SignalsConfirm():
		opmetrics.IdleSignalResult.WithLabelValues(ns, name).Set(2)
		predicted := engine.Predict(0, r.now())
		cpuPerReplica, err := r.metrics.CPURequestPerReplica(ctx, workload)
		if err != nil {
			return nil, fmt.Errorf("reading cpu request for prediction check: %w", err)
		}
		replicas, err := r.metrics.Replicas(ctx, workload)
		if err != nil {
			return nil, fmt.Errorf("reading replicas for prediction check: %w", err)
		}
		totalRequest := cpuPerReplica * float64(replicas)
		predictedPercent := 0.0
		if totalRequest > 0 {
			predictedPercent = predicted / totalRequest * 100
		}
		if predictedPercent >= float64(cpuPercent) {
			opmetrics.IdleFlukes.WithLabelValues(ns, name).Inc()
			r.emitEvent(workload, dryRun, "Normal", ReasonIdleFluke, actionEvaluateIdle,
				"signals confirm idle but prediction disagrees (predicted %.0f%% utilization, threshold %d%%), rechecking",
				predictedPercent, cpuPercent)
			result := ctrl.Result{RequeueAfter: 5 * time.Minute}
			return &result, nil
		}
		r.idle.StartGracePeriod(workload.Namespace, workload.Spec.Target.Name)
		r.emitEvent(workload, dryRun, "Normal", ReasonIdleGracePeriod, actionEvaluateIdle,
			"signals and prediction confirm idle (predicted demand %.0fm), starting grace period",
			predicted)
		result := ctrl.Result{RequeueAfter: 30 * time.Second}
		return &result, nil

	case eval.InGracePeriod():
		opmetrics.IdleSignalResult.WithLabelValues(ns, name).Set(3)
		r.emitEvent(workload, dryRun, "Normal", ReasonIdleGracePeriod, actionEvaluateIdle,
			"in grace period, idle for %s", eval.IdleDuration())
		result := ctrl.Result{RequeueAfter: 30 * time.Second}
		return &result, nil

	case eval.IsIdle():
		opmetrics.IdleSignalResult.WithLabelValues(ns, name).Set(4)
		action := resolveIdleAction(workload)
		opmetrics.IdleDetections.WithLabelValues(string(action), ns, name).Inc()
		r.emitEvent(workload, dryRun, "Normal", ReasonIdleDetected, actionEvaluateIdle,
			"idle for %s, executing %s", eval.IdleDuration(), action)

		if dryRun {
			opmetrics.DryrunActions.WithLabelValues("idle_" + string(action)).Inc()
			r.emitEvent(workload, dryRun, "Normal", ReasonIdleDetected, actionEvaluateIdle,
				"would %s workload (idle for %s)", action, eval.IdleDuration())
			return nil, nil
		}

		if workload.Status.Phase != v1alpha1.PhaseIdle {
			if _, err := r.transition(ctx, workload, v1alpha1.PhaseIdle, "IdleDetected"); err != nil {
				return nil, err
			}
		}

		switch action {
		case v1alpha1.IdleActionDestroy:
			return r.handleDestroy(ctx, workload)
		default:
			return r.handlePause(ctx, workload)
		}

	default:
		opmetrics.IdleSignalResult.WithLabelValues(ns, name).Set(1)
		return nil, nil
	}
}
