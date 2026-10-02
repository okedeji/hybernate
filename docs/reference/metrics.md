# Metrics Reference

All metrics are prefixed with `hybernate_` and registered with the controller-runtime metrics registry. They are served on the metrics endpoint (default `:8443` HTTPS).

## Tier 1: Cluster Health

These metrics show whether the operator is healthy and what phase each workload is in.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hybernate_workload_phase` | Gauge | `namespace`, `workload`, `phase` | 1 for each workload's current lifecycle phase |
| `hybernate_reconcile_errors_total` | Counter | `controller` | Reconciliation errors by controller |
| `hybernate_lifecycle_transitions_total` | Counter | `from`, `to` | Phase transitions |
| `hybernate_lifecycle_action_duration_seconds` | Histogram | `action` | Duration of lifecycle actions (pause, resume, destroy) |

## Tier 2: Operational Insight

These metrics help you understand what the operator is doing and why.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hybernate_prediction_confidence_percent` | Gauge | `season`, `namespace`, `workload` | Prediction accuracy by season |
| `hybernate_prediction_phase` | Gauge | `namespace`, `workload` | Engine phase (0-4) |
| `hybernate_prediction_data_points` | Gauge | `namespace`, `workload` | Data points collected |
| `hybernate_prediction_anomalies_total` | Counter | `namespace`, `workload` | Anomalies detected |
| `hybernate_idle_detections_total` | Counter | `action`, `namespace`, `workload` | Idle detections by action |
| `hybernate_pause_expiry_actions_total` | Counter | `action` | Pause expiry events |
| `hybernate_drift_detections_total` | Counter | `policy` | Paused workloads scaled up externally |

## Tier 3: Debugging

These metrics help troubleshoot specific workload behavior.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hybernate_idle_seconds` | Gauge | `namespace`, `workload` | Seconds since the workload's last activity |
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
# Workloads with low prediction confidence
hybernate_prediction_confidence_percent{season="daily"} < 70

# Workloads that will pause within 10 minutes (with the default 1h idleAfter)
hybernate_idle_seconds > 3000
```
