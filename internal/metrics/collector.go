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

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// --- Tier 1: Cluster Health ---

var (
	// WorkloadPhase is 1 for each workload's current phase, in the style of
	// kube-state-metrics, so alerts can see which workload is in which phase
	// and for how long.
	WorkloadPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hybernate_workload_phase",
		Help: "Current lifecycle phase of each managed workload (1 for the current phase).",
	}, []string{"namespace", "workload", "phase"})

	ReconcileErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hybernate_reconcile_errors_total",
		Help: "Total reconciliation errors by controller.",
	}, []string{"controller"})

	LifecycleTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hybernate_lifecycle_transitions_total",
		Help: "Total lifecycle phase transitions.",
	}, []string{"from", "to"})

	LifecycleActionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "hybernate_lifecycle_action_duration_seconds",
		Help:    "Duration of lifecycle actions (pause, resume, destroy).",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
	}, []string{"action"})
)

// --- Tier 2: Operational Insight ---

var (
	PredictionConfidence = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hybernate_prediction_confidence_percent",
		Help: "Prediction engine confidence percentage by season.",
	}, []string{"season", "namespace", "workload"})

	PredictionPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hybernate_prediction_phase",
		Help: "Prediction engine phase (0=Observing, 1=DailySuggesting, 2=DailyActive, 3=WeeklySuggesting, 4=FullyActive).",
	}, []string{"namespace", "workload"})

	PredictionDataPoints = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hybernate_prediction_data_points",
		Help: "Total data points collected by the prediction engine.",
	}, []string{"namespace", "workload"})

	PredictionAnomalies = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hybernate_prediction_anomalies_total",
		Help: "Total anomalies detected by the prediction engine.",
	}, []string{"namespace", "workload"})

	IdleDetections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hybernate_idle_detections_total",
		Help: "Total idle detections by action taken.",
	}, []string{"action", "namespace", "workload"})

	PauseExpiryActions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hybernate_pause_expiry_actions_total",
		Help: "Total pause expiry events by action (resume or destroy).",
	}, []string{"action"})

	DriftDetections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hybernate_drift_detections_total",
		Help: "Total replica drift detections by conflict policy.",
	}, []string{"policy"})
)

// --- Tier 3: Debugging ---

var (
	IdleSignalResult = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hybernate_idle_signal_result",
		Help: "Current idle signal evaluation status (1=active, 2=signals_confirm, 3=grace_period, 4=idle).",
	}, []string{"namespace", "workload"})

	IdleFlukes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hybernate_idle_fluke_total",
		Help: "Total times signals confirmed idle but prediction disagreed.",
	}, []string{"namespace", "workload"})

	PredictionRegimeChanges = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hybernate_prediction_regime_changes_total",
		Help: "Total regime changes detected by the prediction engine.",
	}, []string{"namespace", "workload"})

	PVCRetentionRemaining = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hybernate_pvc_retention_remaining_seconds",
		Help: "Seconds until PVC retention expires and PVCs are deleted.",
	}, []string{"namespace", "workload"})

	AutomationSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hybernate_automation_skipped_total",
		Help: "Total times automation was skipped due to manual desiredState override.",
	}, []string{"namespace", "workload"})

	DryrunActions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hybernate_dryrun_actions_total",
		Help: "Total actions that would have been taken in dry-run mode.",
	}, []string{"action"})

	TargetUnavailable = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hybernate_target_unavailable_total",
		Help: "Total times the target workload was not found.",
	}, []string{"namespace", "workload"})
)

// --- Discovery ---

var (
	DiscoveryScanDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "hybernate_discovery_scan_duration_seconds",
		Help:    "Duration of namespace discovery scans.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
	})

	DiscoveryWorkloads = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hybernate_discovery_workloads",
		Help: "Number of discovered workloads by classification.",
	}, []string{"classification"})

	DiscoveryAutoManaged = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "hybernate_discovery_auto_managed_total",
		Help: "Total ManagedWorkload CRs auto-created by discovery.",
	})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		// Tier 1
		WorkloadPhase,
		ReconcileErrors,
		LifecycleTransitions,
		LifecycleActionDuration,

		// Tier 2
		PredictionConfidence,
		PredictionPhase,
		PredictionDataPoints,
		PredictionAnomalies,
		IdleDetections,
		PauseExpiryActions,
		DriftDetections,

		// Tier 3
		IdleSignalResult,
		IdleFlukes,
		PredictionRegimeChanges,
		PVCRetentionRemaining,
		AutomationSkipped,
		DryrunActions,
		TargetUnavailable,

		// Discovery
		DiscoveryScanDuration,
		DiscoveryWorkloads,
		DiscoveryAutoManaged,
	)
}
