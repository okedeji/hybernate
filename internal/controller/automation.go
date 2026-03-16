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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/forecast"
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
}

type metricsReader interface {
	CPUUsage(ctx context.Context, workload *v1alpha1.ManagedWorkload) (resource.Quantity, error)
	TotalCPUMillis(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
	CPURequestPerReplica(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
	TotalMemoryBytes(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
	TotalPVCBytes(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error)
}

type idleEvaluator interface {
	Evaluate(ctx context.Context, namespace, name string, signals []signal.Checker, gracePeriod time.Duration) (policy.IdleEvaluation, error)
	StartGracePeriod(namespace, name string)
	Reset(namespace, name string)
}

type scaleEvaluator interface {
	Evaluate(ctx context.Context, namespace, name string, proposed, current int32, constraints policy.ScaleConstraints, signals []signal.Checker) (policy.ScaleDecision, error)
}

type lifecycleScaler interface {
	Scale(ctx context.Context, workload *v1alpha1.ManagedWorkload, target int32) (bool, error)
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

	log := log.FromContext(ctx)
	key := workload.Namespace + "/" + workload.Name
	engine := r.engines.getOrCreate(key, workload.Spec.Prediction.Confidence, predictionState(workload))

	// Feed engine hourly — prediction learns regardless of desiredState.
	if r.metrics != nil && r.engines.shouldFeed(key, r.now()) {
		metric, err := r.metrics.TotalCPUMillis(ctx, workload)
		if err != nil {
			log.Error(err, "reading metrics, will retry")
			result := ctrl.Result{RequeueAfter: 1 * time.Minute}
			return &result, nil
		}
		forecast := engine.Observe(metric, r.now())
		r.engines.markFed(key, r.now())
		r.emitEvent(workload, false, "Normal", ReasonPredictionFed,
			"fed %.0fm CPU, forecast %.0fm, phase %s", metric, forecast, engine.GetPhase())
	}

	// Always update prediction status so the user sees progress.
	r.updatePredictionStatus(workload, engine)

	// If manual desiredState is set, prediction still learns but
	// automation does not act. Status is updated above.
	if workload.Spec.DesiredState != nil {
		r.emitEvent(workload, false, "Normal", ReasonAutomationSkipped,
			"automation skipped, desiredState is manually set to %s", *workload.Spec.DesiredState)
		if err := r.Status().Update(ctx, workload); err != nil {
			return nil, fmt.Errorf("updating prediction status: %w", err)
		}
		result := ctrl.Result{RequeueAfter: 1 * time.Hour}
		return &result, nil
	}

	enginePhase := engine.GetPhase()
	log.Info("automation tick", "engine_phase", enginePhase, "data_points", engine.GetDataPoints())

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

func (r *Reconciler) reconcileAutoResume(ctx context.Context, workload *v1alpha1.ManagedWorkload) (*ctrl.Result, error) {
	if workload.Spec.IdlePolicy == nil || !workload.Spec.IdlePolicy.AutoResume {
		return nil, nil
	}

	// Manual desiredState takes precedence — don't fight the user.
	if workload.Spec.DesiredState != nil {
		return nil, nil
	}

	key := workload.Namespace + "/" + workload.Name
	engine := r.engines.getOrCreate(key, workload.Spec.Prediction.Confidence, predictionState(workload))

	enginePhase := engine.GetPhase()
	if enginePhase == forecast.Observing {
		return nil, nil
	}

	dryRun := enginePhase == forecast.DailySuggesting || workload.Spec.DryRun
	predicted := engine.Predict(0, r.now())
	threshold := idleThresholdFor(workload)

	if predicted < threshold {
		return nil, nil
	}

	r.emitEvent(workload, dryRun, "Normal", ReasonAutoResume,
		"prediction expects demand %.0fm (threshold %.0fm), resuming", predicted, threshold)

	if dryRun {
		return nil, nil
	}

	r.idle.Reset(workload.Namespace, workload.Spec.Target.Name)
	return r.handleResume(ctx, workload)
}

func (r *Reconciler) reconcileAutomationPolicies(ctx context.Context, workload *v1alpha1.ManagedWorkload, engine forecaster, dryRun bool) (*ctrl.Result, error) {
	if workload.Spec.IdlePolicy != nil {
		result, err := r.reconcileIdleAction(ctx, workload, engine, dryRun)
		if err != nil {
			return nil, err
		}
		if result != nil {
			return result, nil
		}
	}

	if workload.Spec.ScalePolicy != nil {
		result, err := r.reconcileScaleAction(ctx, workload, engine, dryRun)
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
	signals := r.buildIdleSignals(workload)
	eval, err := r.idle.Evaluate(ctx, workload.Namespace, workload.Spec.Target.Name, signals, idleGracePeriod(workload))
	if err != nil {
		r.emitEvent(workload, dryRun, "Warning", ReasonIdleConsensus,
			"failed to get idle signal consensus, %v", err)
		return nil, fmt.Errorf("evaluating idle: %w", err)
	}

	switch {
	case eval.SignalsConfirm():
		predicted := engine.Predict(0, r.now())
		if predicted >= idleThresholdFor(workload) {
			r.emitEvent(workload, dryRun, "Normal", ReasonIdleFluke,
				"signals confirm idle but prediction disagrees (predicted demand %.0fm, threshold %.0fm), rechecking",
				predicted, idleThresholdFor(workload))
			result := ctrl.Result{RequeueAfter: 5 * time.Minute}
			return &result, nil
		}
		r.idle.StartGracePeriod(workload.Namespace, workload.Spec.Target.Name)
		r.emitEvent(workload, dryRun, "Normal", ReasonIdleGracePeriod,
			"signals and prediction confirm idle (predicted demand %.0fm), starting grace period",
			predicted)
		result := ctrl.Result{RequeueAfter: 30 * time.Second}
		return &result, nil

	case eval.InGracePeriod():
		r.emitEvent(workload, dryRun, "Normal", ReasonIdleGracePeriod,
			"in grace period, idle for %s", eval.IdleDuration())
		result := ctrl.Result{RequeueAfter: 30 * time.Second}
		return &result, nil

	case eval.IsIdle():
		r.emitEvent(workload, dryRun, "Normal", ReasonIdleDetected,
			"idle for %s, executing %s", eval.IdleDuration(), resolveIdleAction(workload))

		if dryRun {
			return nil, nil
		}

		if workload.Status.Phase != v1alpha1.PhaseIdle {
			if _, err := r.transition(ctx, workload, v1alpha1.PhaseIdle, "IdleDetected"); err != nil {
				return nil, err
			}
		}

		switch resolveIdleAction(workload) {
		case v1alpha1.IdleActionDestroy:
			return r.handleDestroy(ctx, workload)
		default:
			return r.handlePause(ctx, workload)
		}

	default:
		return nil, nil
	}
}

func (r *Reconciler) reconcileScaleAction(ctx context.Context, workload *v1alpha1.ManagedWorkload, engine forecaster, dryRun bool) (*ctrl.Result, error) {
	predicted := engine.Predict(1, r.now())

	cpuPerReplica, err := r.metrics.CPURequestPerReplica(ctx, workload)
	if err != nil {
		r.emitEvent(workload, dryRun, "Warning", ReasonScalingUnavailable,
			"cannot compute replica count, %v", err)
		return nil, fmt.Errorf("reading cpu request per replica: %w", err)
	}

	sp := workload.Spec.ScalePolicy
	proposed := demandToReplicas(predicted, cpuPerReplica, sp.MinReplicas, sp.MaxReplicas)

	constraints := policy.ScaleConstraints{
		MinReplicas: int32(sp.MinReplicas),
		MaxReplicas: int32(sp.MaxReplicas),
	}
	if sp.Down != nil {
		if sp.Down.Stabilization != nil {
			constraints.DownStabilization = sp.Down.Stabilization.Duration
		}
		if sp.Down.MaxStep != nil {
			constraints.MaxStepDown = int32(*sp.Down.MaxStep)
		}
	}
	if sp.Up != nil {
		if sp.Up.Stabilization != nil {
			constraints.UpStabilization = sp.Up.Stabilization.Duration
		}
		if sp.Up.MaxStep != nil {
			constraints.MaxStepUp = int32(*sp.Up.MaxStep)
		}
	}

	current := proposed
	if workload.Status.Scale != nil {
		current = workload.Status.Scale.CurrentReplicas
	}

	decision, err := r.scale.Evaluate(ctx, workload.Namespace, workload.Spec.Target.Name, proposed, current, constraints, nil)
	if err != nil {
		return nil, fmt.Errorf("evaluating scale: %w", err)
	}

	if !decision.ShouldScale() {
		return nil, nil
	}

	target := decision.GetTarget()

	if decision.Direction == policy.ScaleDown {
		guards := r.buildScaleDownGuards(workload, float64(target)*cpuPerReplica)
		res, err := signal.CheckAll(ctx, workload.Namespace, workload.Spec.Target.Name, guards)
		if err != nil {
			return nil, fmt.Errorf("checking scale-down guards: %w", err)
		}
		if !res.Confirm {
			r.emitEvent(workload, dryRun, "Normal", ReasonScaleDownGuarded,
				"scale-down to %d blocked: %s", target, res.Reason)
			result := ctrl.Result{RequeueAfter: 1 * time.Minute}
			return &result, nil
		}
	}

	if dryRun {
		r.emitEvent(workload, dryRun, "Normal", ReasonScaled,
			"would scale to %d replicas (predicted demand %.0fm, cpu/replica %.0fm)",
			target, predicted, cpuPerReplica)
		return nil, nil
	}

	if _, err := r.transition(ctx, workload, v1alpha1.PhaseScaling, "ScaleDecision"); err != nil {
		return nil, err
	}

	done, err := r.lifecycleScaler.Scale(ctx, workload, target)
	if err != nil {
		return nil, fmt.Errorf("scaling workload: %w", err)
	}

	if !done {
		result := ctrl.Result{RequeueAfter: 5 * time.Second}
		return &result, nil
	}

	r.stampLastActed(workload)
	r.emitEvent(workload, false, "Normal", ReasonScaled,
		"scaled to %d replicas", target)

	result, err := r.transition(ctx, workload, v1alpha1.PhaseRunning, "ScaleComplete")
	if err != nil {
		return nil, err
	}
	return &result, nil
}
