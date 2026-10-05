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

	"github.com/prometheus/client_golang/prometheus"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
	"github.com/okedeji/hybernate/internal/forecast"
	opmetrics "github.com/okedeji/hybernate/internal/metrics"
)

const (
	ReasonPredictionFed          = "PredictionFed"
	ReasonAutomationSkipped      = "AutomationSkipped"
	ReasonIdleDetected           = "IdleDetected"
	ReasonIdleVetoed             = "IdleVetoed"
	ReasonActivityResumed        = "ActivityResumed"
	ReasonWokeByActivity         = "WokeByActivity"
	ReasonPaused                 = "Paused"
	ReasonResumed                = "Resumed"
	ReasonAutoResume             = "AutoResume"
	ReasonDependenciesLearned    = "DependenciesLearned"
	ReasonProtected              = "Protected"
	ReasonScaledUp               = "ScaledUp"
	ReasonGitOpsConflict         = "GitOpsConflict"
	ReasonGitOpsConflictResolved = "GitOpsConflictResolved"
	ReasonRegimeChange           = "RegimeChange"
	ReasonTargetNotFound         = "TargetNotFound"
	ReasonForecastReset          = "ForecastReset"
)

// Actions populate the events.k8s.io/v1 Action field, which the API server
// requires: what the operator did or tried to do when the event fired.
const (
	actionForecast          = "Forecast"
	actionEvaluate          = "EvaluateAutomation"
	actionEvaluateIdle      = "EvaluateIdle"
	actionPause             = "Pause"
	actionResume            = "Resume"
	actionCheckTarget       = "CheckTarget"
	actionCheckReplicas     = "CheckReplicas"
	actionLearnDependencies = "LearnDependencies"
	actionCheckNamespace    = "CheckNamespace"
	actionCheckDuplicate    = "CheckDuplicate"
	actionAutoManage        = "AutoManage"
	actionScan              = "Scan"
)

func (r *Reconciler) emitEvent(workload *v1alpha1.ManagedWorkload, dryRun bool, eventType, reason, action, msgFmt string, args ...any) {
	msg := fmt.Sprintf(msgFmt, args...)
	r.Recorder.Eventf(workload, nil, eventType, reason, action,
		"%s%s: %s", dryRunPrefix(dryRun), workload.Spec.Target.Name, msg)
}

func dryRunPrefix(dryRun bool) string {
	if dryRun {
		return "[dry-run] "
	}
	return ""
}

// forecastEngine is the workload's forecast engine, with the settings it
// asks for applied. The first time, it is restored from the state in the
// workload's status; state that can't be restored is discarded, and the
// engine starts learning again.
func (r *Reconciler) forecastEngine(workload *v1alpha1.ManagedWorkload) forecaster {
	var state string
	if p := workload.Status.Prediction; p != nil {
		state = p.State
	}
	settings := forecast.Settings{Threshold: workload.Spec.Prediction.Confidence, Location: r.Timezone}
	engine, err := r.engines.getOrCreate(workload.UID, settings, state)
	if err != nil {
		r.emitEvent(workload, false, "Warning", ReasonForecastReset, actionForecast,
			"the forecast's saved state can't be read, so it starts learning again: %v", err)
	}
	return engine
}

// updatePredictionStatus publishes the engine's phase and confidence, and
// the state it has learned. The state only changes when an hour is
// observed, so it is written once an hour; a write that fails is made again
// from the engine on the next reconcile.
func (r *Reconciler) updatePredictionStatus(ctx context.Context, workload *v1alpha1.ManagedWorkload, engine forecaster) {
	phase := engine.GetPhase()
	dailyPhase, weeklyPhase := seasonPhases(phase)

	status := &v1alpha1.PredictionStatus{
		DailyPhase:       dailyPhase,
		DailyConfidence:  engine.DailyConfidence(),
		WeeklyPhase:      weeklyPhase,
		WeeklyConfidence: engine.WeeklyConfidence(),
	}
	state, err := engine.Export()
	if err != nil {
		logf.FromContext(ctx).Error(err, "saving forecast state, keeping the state last saved",
			"workload", workload.Name, "namespace", workload.Namespace)
		if last := workload.Status.Prediction; last != nil {
			state = last.State
		}
	}
	status.State = state
	workload.Status.Prediction = status

	ns, name := workload.Namespace, workload.Name
	opmetrics.PredictionConfidence.WithLabelValues("daily", ns, name).Set(float64(engine.DailyConfidence()))
	opmetrics.PredictionConfidence.WithLabelValues("weekly", ns, name).Set(float64(engine.WeeklyConfidence()))
	opmetrics.PredictionPhase.WithLabelValues(ns, name).Set(float64(phase))
	opmetrics.PredictionDataPoints.WithLabelValues(ns, name).Set(float64(engine.GetDataPoints()))
}

// forgetForecast drops a deleted workload's engine and its metric series.
func (r *Reconciler) forgetForecast(workload *v1alpha1.ManagedWorkload) {
	r.engines.forget(workload.UID)
	labels := prometheus.Labels{"namespace": workload.Namespace, "workload": workload.Name}
	opmetrics.PredictionConfidence.DeletePartialMatch(labels)
	opmetrics.PredictionPhase.DeletePartialMatch(labels)
	opmetrics.PredictionDataPoints.DeletePartialMatch(labels)
	opmetrics.PredictionAnomalies.DeletePartialMatch(labels)
	opmetrics.PredictionRegimeChanges.DeletePartialMatch(labels)
}

const (
	seasonObserving  = "Observing"
	seasonSuggesting = "Suggesting"
	seasonActive     = "Active"
	seasonUnknown    = "Unknown"
)

func seasonPhases(phase forecast.Phase) (daily, weekly string) {
	switch phase {
	case forecast.Observing:
		return seasonObserving, seasonObserving
	case forecast.DailySuggesting:
		return seasonSuggesting, seasonObserving
	case forecast.DailyActive:
		return seasonActive, seasonObserving
	case forecast.WeeklySuggesting:
		return seasonActive, seasonSuggesting
	case forecast.FullyActive:
		return seasonActive, seasonActive
	default:
		return seasonUnknown, seasonUnknown
	}
}

func (r *Reconciler) stampLastActed(workload *v1alpha1.ManagedWorkload) {
	now := r.clockTime()
	workload.Status.LastActedAt = &now
}

// resolveCostRates is what a workload is priced at: the rates it sets in
// costTracking.rates, then the list rates of the nodes it ran on, then the
// defaults, each part by part.
func resolveCostRates(workload *v1alpha1.ManagedWorkload) cost.Rates {
	rates := cost.DefaultRates
	if c := workload.Status.Cost; c != nil {
		rates = withRates(rates, c.ListRates)
	}
	if workload.Spec.CostTracking != nil {
		rates = withRates(rates, workload.Spec.CostTracking.Rates)
	}
	return rates
}

func withRates(rates cost.Rates, r *v1alpha1.CostRates) cost.Rates {
	if r == nil {
		return rates
	}
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
