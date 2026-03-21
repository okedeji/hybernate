# Metrics Reference

All metrics are prefixed with `hybernate_` and registered with the controller-runtime metrics registry. They are served on the metrics endpoint (default `:8443` HTTPS).

## Tier 1: Cluster Health

These metrics provide a high-level view of Hybernate's impact.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hybernate_workloads_total` | Gauge | `phase` | Managed workloads by lifecycle phase |
| `hybernate_active_workloads` | Gauge | | Workloads in a running state |
| `hybernate_paused_workloads` | Gauge | | Currently paused workloads |
| `hybernate_destroyed_workloads` | Gauge | | Destroyed workloads |
| `hybernate_reconcile_errors_total` | Counter | `controller` | Reconciliation errors by controller |
| `hybernate_lifecycle_transitions_total` | Counter | `from`, `to` | Phase transitions |
| `hybernate_lifecycle_action_duration_seconds` | Histogram | `action` | Duration of lifecycle actions (pause, resume, destroy, scale) |
| `hybernate_cost_estimated_savings_dollars` | Gauge | | Estimated monthly savings (requires autoscaler for realization) |
| `hybernate_cost_estimated_dollars` | Gauge | | Total estimated monthly cost |
| `hybernate_cost_estimated_without_management_dollars` | Gauge | | Estimated cost without Hybernate |
| `hybernate_resource_reduction_cpu_millicores` | Gauge | | Total CPU millicores freed by Hybernate |
| `hybernate_resource_reduction_memory_bytes` | Gauge | | Total memory bytes freed by Hybernate |

## Tier 2: Operational Insight

These metrics help you understand what the operator is doing and why.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hybernate_prediction_confidence_percent` | Gauge | `season`, `namespace`, `workload` | Prediction accuracy by season |
| `hybernate_prediction_phase` | Gauge | `namespace`, `workload` | Engine phase (0-4) |
| `hybernate_prediction_data_points` | Gauge | `namespace`, `workload` | Data points collected |
| `hybernate_prediction_anomalies_total` | Counter | `namespace`, `workload` | Anomalies detected |
| `hybernate_cost_cpu_hours` | Gauge | | Total vCPU-hours this month |
| `hybernate_cost_memory_hours` | Gauge | | Total GiB memory hours |
| `hybernate_cost_storage_hours` | Gauge | | Total GiB storage hours |
| `hybernate_scale_events_total` | Counter | `direction`, `namespace`, `workload` | Scale events by direction |
| `hybernate_scale_replicas` | Gauge | `namespace`, `workload` | Current replica count |
| `hybernate_idle_detections_total` | Counter | `action`, `namespace`, `workload` | Idle detections by action |
| `hybernate_pause_expiry_actions_total` | Counter | `action` | Pause expiry events |
| `hybernate_drift_detections_total` | Counter | `policy` | Replica drift detections |

## Tier 3: Debugging

These metrics help troubleshoot specific workload behavior.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hybernate_idle_signal_result` | Gauge | `namespace`, `workload` | Idle signal status (1=active, 2=signals_confirm, 3=grace_period, 4=idle) |
| `hybernate_scale_guard_blocked_total` | Counter | `namespace`, `workload` | Scale-downs blocked by guard probes |
| `hybernate_idle_fluke_total` | Counter | `namespace`, `workload` | Signals confirmed idle but prediction disagreed |
| `hybernate_prediction_regime_changes_total` | Counter | `namespace`, `workload` | Regime changes detected |
| `hybernate_pvc_retention_remaining_seconds` | Gauge | `namespace`, `workload` | Seconds until PVC cleanup |
| `hybernate_automation_skipped_total` | Counter | `namespace`, `workload` | Automation skipped (manual override active) |
| `hybernate_dryrun_actions_total` | Counter | `action` | Actions that would have been taken in dry-run |
| `hybernate_target_unavailable_total` | Counter | `namespace`, `workload` | Target workload not found |

## Discovery

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hybernate_discovery_scan_duration_seconds` | Histogram | | Scan duration |
| `hybernate_discovery_workloads` | Gauge | `classification` | Discovered workloads by class |
| `hybernate_discovery_estimated_savings_dollars` | Gauge | | Estimated savings from discoveries |
| `hybernate_discovery_auto_managed_total` | Counter | | Auto-created ManagedWorkloads |

## Prediction Phase Values

The `hybernate_prediction_phase` gauge uses numeric values:

| Value | Phase |
|-------|-------|
| 0 | Observing |
| 1 | DailySuggesting |
| 2 | DailyActive |
| 3 | WeeklySuggesting |
| 4 | FullyActive |

## Useful PromQL Queries

```promql
# Total workloads by phase
hybernate_workloads_total

# Estimated monthly savings trend
hybernate_cost_estimated_savings_dollars

# Workloads with low prediction confidence
hybernate_prediction_confidence_percent{season="daily"} < 70

# Recent scale events
rate(hybernate_scale_events_total[1h])

# Idle flukes (signals say idle, prediction disagrees)
rate(hybernate_idle_fluke_total[1h]) > 0

# Workloads stuck in grace period
hybernate_idle_signal_result == 3
```
