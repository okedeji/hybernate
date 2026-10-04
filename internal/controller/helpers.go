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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
	ReasonScaledUp               = "ScaledUp"
	ReasonGitOpsConflict         = "GitOpsConflict"
	ReasonGitOpsConflictResolved = "GitOpsConflictResolved"
	ReasonRegimeChange           = "RegimeChange"
	ReasonTargetNotFound         = "TargetNotFound"
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

func (r *Reconciler) predictionState(ctx context.Context, workload *v1alpha1.ManagedWorkload) *string {
	var cm corev1.ConfigMap
	key := client.ObjectKey{
		Namespace: workload.Namespace,
		Name:      predictionConfigMapName(workload.Name),
	}
	if err := r.Get(ctx, key, &cm); err != nil {
		return nil
	}
	if s, ok := cm.Data["state"]; ok {
		return &s
	}
	return nil
}

func predictionConfigMapName(workloadName string) string {
	return workloadName + "-prediction-state"
}

func (r *Reconciler) updatePredictionStatus(ctx context.Context, workload *v1alpha1.ManagedWorkload, engine forecaster) {
	phase := engine.GetPhase()
	dailyPhase, weeklyPhase := seasonPhases(phase)

	workload.Status.Prediction = &v1alpha1.PredictionStatus{
		DailyPhase:       dailyPhase,
		DailyConfidence:  engine.DailyConfidence(),
		WeeklyPhase:      weeklyPhase,
		WeeklyConfidence: engine.WeeklyConfidence(),
	}

	if data, err := engine.Export(); err == nil {
		r.savePredictionState(ctx, workload, string(data))
	}

	ns, name := workload.Namespace, workload.Name
	opmetrics.PredictionConfidence.WithLabelValues("daily", ns, name).Set(float64(engine.DailyConfidence()))
	opmetrics.PredictionConfidence.WithLabelValues("weekly", ns, name).Set(float64(engine.WeeklyConfidence()))
	opmetrics.PredictionPhase.WithLabelValues(ns, name).Set(float64(phase))
	opmetrics.PredictionDataPoints.WithLabelValues(ns, name).Set(float64(engine.GetDataPoints()))
}

func (r *Reconciler) savePredictionState(ctx context.Context, workload *v1alpha1.ManagedWorkload, state string) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      predictionConfigMapName(workload.Name),
			Namespace: workload.Namespace,
		},
	}
	_, err := ctrlutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data["state"] = state
		return ctrlutil.SetOwnerReference(workload, cm, r.Scheme)
	})
	if err != nil {
		logf.FromContext(ctx).Error(err, "saving prediction state", "configmap", cm.Name)
	}
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

func parseDollarAmount(s string) float64 {
	if len(s) < 2 || s[0] != '$' {
		return 0
	}
	var v float64
	_, _ = fmt.Sscanf(s[1:], "%f", &v)
	return v
}
