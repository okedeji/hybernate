# Metrics Reference

All metrics are prefixed with `hybernate_` and registered with the controller-runtime metrics registry. They are served on the metrics endpoint, which the chart and the kustomize manifests serve on `:8443` over HTTPS; a scraper needs a token allowed to `get` `/metrics` (see [Monitoring](../operations/monitoring.md#prometheus)). Per-workload series are dropped when their ManagedWorkload is deleted.

## Tier 1: Cluster Health

These metrics show whether the operator is healthy and what phase each workload is in.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hybernate_workload_phase` | Gauge | `namespace`, `workload`, `phase` | 1 for each workload's current lifecycle phase |
| `hybernate_reconcile_errors_total` | Counter | `controller` | Reconciliation errors by controller |
| `hybernate_lifecycle_transitions_total` | Counter | `from`, `to` | Phase transitions |
| `hybernate_lifecycle_action_duration_seconds` | Histogram | `action` | Duration of lifecycle actions (pause, resume) |

## Tier 2: Operational Insight

These metrics help you understand what the operator is doing and why.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hybernate_prediction_confidence_percent` | Gauge | `season`, `namespace`, `workload` | Forecast confidence, 1 − WAPE, as a percentage, for `season` `daily` or `weekly` |
| `hybernate_prediction_phase` | Gauge | `namespace`, `workload` | Engine phase (0-4) |
| `hybernate_prediction_data_points` | Gauge | `namespace`, `workload` | Data points collected |
| `hybernate_prediction_anomalies_total` | Counter | `namespace`, `workload` | Anomalies detected |
| `hybernate_idle_detections_total` | Counter | `namespace`, `workload` | Times each workload's idle clock ran out |
| `hybernate_external_scale_ups_total` | Counter | `by` | Paused workloads scaled up outside Hybernate, by `argo-cd`, `flux`, or `other` |

## Tier 3: Debugging

These metrics help troubleshoot specific workload behavior.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hybernate_idle_seconds` | Gauge | `namespace`, `workload` | Seconds since the workload's last activity |
| `hybernate_prediction_regime_changes_total` | Counter | `namespace`, `workload` | Regime changes detected |
| `hybernate_automation_skipped_total` | Counter | `namespace`, `workload` | Automation skipped (manual override active) |
| `hybernate_dryrun_actions_total` | Counter | `action` | Actions that would have been taken in dry-run; `action` is `idle_pause` |
| `hybernate_target_unavailable_total` | Counter | `namespace`, `workload` | Target workload not found |

## Doorman

Served by the doorman pods, on the same port and path as the operator's metrics.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hybernate_doorman_wakes_total` | Counter | `namespace`, `workload`, `result` | Connections the doorman accepted for a paused workload, by `result` (below) |
| `hybernate_doorman_wait_seconds` | Histogram | `result` | How long the doorman held a connection before passing it through or closing it |
| `hybernate_doorman_held_connections` | Gauge | | Connections held right now, waiting for their workload to wake |
| `hybernate_doorman_proxied_connections` | Gauge | | Connections passed to a woken workload and still being carried |
| `hybernate_doorman_port_conflicts` | Gauge | | Doorman ports claimed by more than one route, which the doorman refuses to serve. Should be 0 |

| `result` | Meaning |
|----------|---------|
| `success` | Passed to a Ready pod |
| `page` | A browser was shown the waking-up page |
| `timeout` | No pod was Ready within `wake.maxWait` |
| `error` | The woken pods couldn't be reached, or the wake couldn't be recorded, by `wake.maxWait` |
| `canceled` | The caller left, or the doorman shut down, while the connection was held |
| `ignored` | A health check or scraper, recognised by its `User-Agent`, or a connection that closed without sending anything |
| `limited` | Refused by a cap on held connections or a wake rate limit; see [Limits](../concepts/wake-on-request.md#limits) |
| `refused` | The workload's route is draining and none of its pods is Ready |

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

# How long a request to a paused workload waits for it to wake (p95)
histogram_quantile(0.95, sum by (le) (rate(hybernate_doorman_wait_seconds_bucket{result="success"}[1h])))
```
