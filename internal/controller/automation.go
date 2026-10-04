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

// metricsReader measures a workload two ways. Activity compares the usage
// of the workload's own containers with their requests, so an injected
// sidecar's background work doesn't keep it awake. Cost counts the whole
// pod, sidecars included, since that's what the workload costs and what
// pausing frees.
type metricsReader interface {
	WorkloadCPUMillis(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
	CPURequestPerReplica(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
	TotalCPUMillis(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
	TotalMemoryBytes(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
	PodRequestsPerReplica(ctx context.Context, workload *v1alpha1.ManagedWorkload) (cpuMillis, memBytes float64, err error)
	Replicas(ctx context.Context, workload *v1alpha1.ManagedWorkload) (int32, error)
	TotalPVCBytes(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
}

// listPricer prices a workload at the nodes it runs on.
type listPricer interface {
	ListRates(ctx context.Context, workload *v1alpha1.ManagedWorkload) (cost.Rates, bool, error)
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

	key := workload.Namespace + "/" + workload.Name
	engine := r.engines.getOrCreate(key, workload.Spec.Prediction.Confidence, r.predictionState(ctx, workload))
	r.observePausedHour(ctx, workload, key, engine)

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
func (r *Reconciler) observePausedHour(ctx context.Context, workload *v1alpha1.ManagedWorkload, key string, engine forecaster) {
	if !meta.IsStatusConditionTrue(workload.Status.Conditions, conditionWakeOnRequest) {
		return
	}
	now := r.now()
	if !r.engines.shouldFeed(key, now) {
		return
	}
	engine.Observe(0, now)
	r.engines.markFed(key, now)
	r.updatePredictionStatus(ctx, workload, engine)
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
