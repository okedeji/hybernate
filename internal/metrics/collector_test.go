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
	"testing"

	"github.com/stretchr/testify/assert"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

func TestAllMetricsRegistered(t *testing.T) {
	expected := []string{
		// Tier 1
		"hybernate_workloads_total",
		"hybernate_active_workloads",
		"hybernate_paused_workloads",
		"hybernate_destroyed_workloads",
		"hybernate_reconcile_errors_total",
		"hybernate_lifecycle_transitions_total",
		"hybernate_lifecycle_action_duration_seconds",
		"hybernate_cost_estimated_savings_dollars",
		"hybernate_cost_estimated_dollars",
		"hybernate_cost_estimated_without_management_dollars",
		"hybernate_resource_reduction_cpu_millicores",
		"hybernate_resource_reduction_memory_bytes",

		// Tier 2
		"hybernate_prediction_confidence_percent",
		"hybernate_prediction_phase",
		"hybernate_prediction_data_points",
		"hybernate_prediction_anomalies_total",
		"hybernate_cost_cpu_hours",
		"hybernate_cost_memory_hours",
		"hybernate_cost_storage_hours",
		"hybernate_scale_events_total",
		"hybernate_scale_replicas",
		"hybernate_idle_detections_total",
		"hybernate_pause_expiry_actions_total",
		"hybernate_drift_detections_total",

		// Tier 3
		"hybernate_idle_signal_result",
		"hybernate_scale_guard_blocked_total",
		"hybernate_idle_fluke_total",
		"hybernate_prediction_regime_changes_total",
		"hybernate_pvc_retention_remaining_seconds",
		"hybernate_automation_skipped_total",
		"hybernate_dryrun_actions_total",
		"hybernate_target_unavailable_total",

		// Discovery
		"hybernate_discovery_scan_duration_seconds",
		"hybernate_discovery_workloads",
		"hybernate_discovery_estimated_savings_dollars",
		"hybernate_discovery_auto_managed_total",
	}

	gathered, err := ctrlmetrics.Registry.Gather()
	assert.NoError(t, err)

	registered := make(map[string]bool)
	for _, mf := range gathered {
		registered[mf.GetName()] = true
	}

	// Metrics without observations won't appear in Gather(), so we verify
	// they're at least discoverable by writing a value and re-gathering.
	WorkloadsTotal.WithLabelValues("test").Set(1)
	ReconcileErrors.WithLabelValues("test").Inc()
	LifecycleTransitions.WithLabelValues("a", "b").Inc()
	LifecycleActionDuration.WithLabelValues("test").Observe(1)
	PredictionConfidence.WithLabelValues("daily", "ns", "w").Set(50)
	PredictionPhase.WithLabelValues("ns", "w").Set(1)
	PredictionDataPoints.WithLabelValues("ns", "w").Set(10)
	PredictionAnomalies.WithLabelValues("ns", "w").Inc()
	ScaleEvents.WithLabelValues("up", "ns", "w").Inc()
	ScaleReplicas.WithLabelValues("ns", "w").Set(3)
	IdleDetections.WithLabelValues("pause", "ns", "w").Inc()
	PauseExpiryActions.WithLabelValues("resume").Inc()
	DriftDetections.WithLabelValues("adopt").Inc()
	IdleSignalResult.WithLabelValues("ns", "w").Set(1)
	ScaleGuardBlocked.WithLabelValues("ns", "w").Inc()
	IdleFlukes.WithLabelValues("ns", "w").Inc()
	PredictionRegimeChanges.WithLabelValues("ns", "w").Inc()
	PVCRetentionRemaining.WithLabelValues("ns", "w").Set(3600)
	AutomationSkipped.WithLabelValues("ns", "w").Inc()
	DryrunActions.WithLabelValues("scale_up").Inc()
	TargetUnavailable.WithLabelValues("ns", "w").Inc()
	DiscoveryScanDuration.Observe(1.5)
	DiscoveryWorkloads.WithLabelValues("Idle").Set(5)
	DiscoveryEstimatedSavings.Set(100)
	DiscoveryAutoManaged.Inc()

	gathered, err = ctrlmetrics.Registry.Gather()
	assert.NoError(t, err)

	registered = make(map[string]bool)
	for _, mf := range gathered {
		registered[mf.GetName()] = true
	}

	for _, name := range expected {
		assert.True(t, registered[name], "metric %s not registered", name)
	}
}
