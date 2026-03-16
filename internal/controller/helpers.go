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
	"fmt"
	"math"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
	"github.com/okedeji/hybernate/internal/forecast"
	opmetrics "github.com/okedeji/hybernate/internal/metrics"
	"github.com/okedeji/hybernate/internal/signal"
)

const (
	ReasonPredictionFed        = "PredictionFed"
	ReasonAutomationSkipped    = "AutomationSkipped"
	ReasonIdleConsensus        = "IdleConsensus"
	ReasonIdleFluke            = "IdleFluke"
	ReasonIdleGracePeriod      = "IdleGracePeriod"
	ReasonIdleDetected         = "IdleDetected"
	ReasonScalingUnavailable   = "ScalingUnavailable"
	ReasonScaleDownGuarded     = "ScaleDownGuarded"
	ReasonScaled               = "Scaled"
	ReasonPaused               = "Paused"
	ReasonResumed              = "Resumed"
	ReasonDestroyed            = "Destroyed"
	ReasonPauseExpired         = "PauseExpired"
	ReasonPVCsCleaned          = "PVCsCleaned"
	ReasonPVCRetentionExpiring = "PVCRetentionExpiring"
	ReasonAutoResume           = "AutoResume"
	ReasonDriftDetected        = "DriftDetected"
	ReasonDriftCorrected       = "DriftCorrected"
)

func (r *Reconciler) emitEvent(workload *v1alpha1.ManagedWorkload, dryRun bool, eventType, reason, msgFmt string, args ...interface{}) {
	msg := fmt.Sprintf(msgFmt, args...)
	r.Recorder.Event(workload, eventType, reason,
		fmt.Sprintf("%s%s: %s", dryRunPrefix(dryRun), workload.Spec.Target.Name, msg))
}

func dryRunPrefix(dryRun bool) string {
	if dryRun {
		return "[dry-run] "
	}
	return ""
}

func resolveIdleAction(workload *v1alpha1.ManagedWorkload) v1alpha1.IdleAction {
	if workload.Spec.IdlePolicy.Action == v1alpha1.IdleActionDestroy {
		return v1alpha1.IdleActionDestroy
	}
	// auto and pause both pause. Destruction after idle is handled by
	// pause.expireAfter + expireAction: destroy.
	return v1alpha1.IdleActionPause
}

const defaultIdleThreshold = 50

func idleThresholdFor(workload *v1alpha1.ManagedWorkload) float64 {
	if workload.Spec.IdlePolicy != nil && workload.Spec.IdlePolicy.IdleThreshold > 0 {
		return float64(workload.Spec.IdlePolicy.IdleThreshold)
	}
	return defaultIdleThreshold
}

func idleGracePeriod(workload *v1alpha1.ManagedWorkload) time.Duration {
	if workload.Spec.IdlePolicy != nil && workload.Spec.IdlePolicy.GracePeriod != nil {
		return workload.Spec.IdlePolicy.GracePeriod.Duration
	}
	return 0
}

func predictionState(workload *v1alpha1.ManagedWorkload) *string {
	if workload.Status.Prediction == nil {
		return nil
	}
	return workload.Status.Prediction.State
}

func (r *Reconciler) updatePredictionStatus(workload *v1alpha1.ManagedWorkload, engine forecaster) {
	phase := engine.GetPhase()
	dailyPhase, weeklyPhase := seasonPhases(phase)

	workload.Status.Prediction = &v1alpha1.PredictionStatus{
		DailyPhase:       dailyPhase,
		DailyConfidence:  engine.DailyConfidence(),
		WeeklyPhase:      weeklyPhase,
		WeeklyConfidence: engine.WeeklyConfidence(),
	}

	if data, err := engine.Export(); err == nil {
		s := string(data)
		workload.Status.Prediction.State = &s
	}

	ns, name := workload.Namespace, workload.Name
	opmetrics.PredictionConfidence.WithLabelValues("daily", ns, name).Set(float64(engine.DailyConfidence()))
	opmetrics.PredictionConfidence.WithLabelValues("weekly", ns, name).Set(float64(engine.WeeklyConfidence()))
	opmetrics.PredictionPhase.WithLabelValues(ns, name).Set(float64(phase))
	opmetrics.PredictionDataPoints.WithLabelValues(ns, name).Set(float64(engine.GetDataPoints()))
}

func seasonPhases(phase forecast.Phase) (daily, weekly string) {
	switch phase {
	case forecast.Observing:
		return "Observing", "Observing"
	case forecast.DailySuggesting:
		return "Suggesting", "Observing"
	case forecast.DailyActive:
		return "Active", "Observing"
	case forecast.WeeklySuggesting:
		return "Active", "Suggesting"
	case forecast.FullyActive:
		return "Active", "Active"
	default:
		return "Unknown", "Unknown"
	}
}

func (r *Reconciler) buildIdleSignals(workload *v1alpha1.ManagedWorkload) []signal.Checker {
	threshold := resource.NewMilliQuantity(int64(idleThresholdFor(workload)), resource.DecimalSI)
	checkers := []signal.Checker{
		signal.NewInternal(r.metrics, workload, *threshold, signal.Below),
	}
	return r.appendUserSignals(checkers, workload.Spec.IdlePolicy.Signals)
}

func (r *Reconciler) buildScaleDownGuards(workload *v1alpha1.ManagedWorkload, targetCapacity float64) []signal.Checker {
	threshold := resource.NewMilliQuantity(int64(targetCapacity), resource.DecimalSI)
	checkers := []signal.Checker{
		signal.NewInternal(r.metrics, workload, *threshold, signal.Below),
	}
	if sp := workload.Spec.ScalePolicy; sp != nil && sp.Down != nil {
		checkers = r.appendUserSignals(checkers, sp.Down.Guard)
	}
	return checkers
}

func (r *Reconciler) appendUserSignals(checkers []signal.Checker, specs []v1alpha1.ProbeSpec) []signal.Checker {
	for _, s := range specs {
		switch s.Source {
		case v1alpha1.ProbeSourcePrometheus:
			checkers = append(checkers, &signal.Prometheus{
				Endpoint: r.prometheusURL,
				Query:    s.PromQL,
			})
		}
	}
	return checkers
}

func resolveConflictAction(workload *v1alpha1.ManagedWorkload) v1alpha1.ConflictAction {
	if workload.Spec.ConflictAction != "" {
		return workload.Spec.ConflictAction
	}
	return v1alpha1.ConflictActionWarn
}

func (r *Reconciler) stampLastActed(workload *v1alpha1.ManagedWorkload) {
	now := r.clockTime()
	workload.Status.LastActedAt = &now
}

func demandToReplicas(demand, cpuPerReplica float64, min, max int) int32 {
	if demand <= 0 || cpuPerReplica <= 0 {
		return int32(min)
	}
	replicas := int32(math.Ceil(demand / cpuPerReplica))
	if replicas < int32(min) {
		return int32(min)
	}
	if replicas > int32(max) {
		return int32(max)
	}
	return replicas
}

func resolveCostRates(workload *v1alpha1.ManagedWorkload) cost.Rates {
	rates := cost.DefaultRates
	if workload.Spec.CostTracking == nil || workload.Spec.CostTracking.Rates == nil {
		return rates
	}
	r := workload.Spec.CostTracking.Rates
	if r.CPUPerHour != nil {
		rates.CPUPerHour = r.CPUPerHour.AsApproximateFloat64()
	}
	if r.MemoryPerHour != nil {
		rates.MemoryPerHour = r.MemoryPerHour.AsApproximateFloat64()
	}
	if r.StoragePerMonth != nil {
		rates.StoragePerMonth = r.StoragePerMonth.AsApproximateFloat64()
	}
	return rates
}

func parseDollarAmount(s string) float64 {
	if len(s) < 2 || s[0] != '$' {
		return 0
	}
	var v float64
	fmt.Sscanf(s[1:], "%f", &v)
	return v
}
