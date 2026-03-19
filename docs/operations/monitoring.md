# Monitoring

Hybernate ships with Prometheus metrics, Grafana dashboards, and alerting rules.

## Prometheus

### ServiceMonitor

The project includes a ServiceMonitor in `config/prometheus/` for automatic Prometheus scraping:

```bash
kubectl apply -f config/prometheus/monitor.yaml
```

This configures Prometheus to scrape the operator's metrics endpoint.

### Key Metrics to Watch

**Cluster health dashboard:**

```promql
# Are workloads being managed?
hybernate_workloads_total

# Is the operator saving money?
hybernate_cost_savings_dollars

# Are there errors?
rate(hybernate_reconcile_errors_total[5m]) > 0
```

**Per-workload health:**

```promql
# Is the prediction engine learning?
hybernate_prediction_confidence_percent{season="daily"}

# Are workloads cycling too fast?
rate(hybernate_lifecycle_transitions_total[1h])

# Are scale-downs being blocked?
rate(hybernate_scale_guard_blocked_total[1h])
```

## Grafana Dashboards

Pre-built dashboards are available in `config/grafana/`:

- **Hybernate Overview**: cluster-wide workload counts, cost savings, phase distribution
- **Workload Detail**: per-workload prediction confidence, scaling history, idle detection state

Import them via Grafana's dashboard import feature or deploy them as ConfigMaps if using the Grafana sidecar.

## Alerting Rules

Sample alerting rules are in `config/prometheus/`:

### Suggested Alerts

| Alert | Condition | Severity |
|-------|-----------|----------|
| HybernateReconcileErrors | `rate(hybernate_reconcile_errors_total[5m]) > 0` | warning |
| HybernatePredictionLowConfidence | `hybernate_prediction_confidence_percent{season="daily"} < 50` for 1h | info |
| HybernateTargetUnavailable | `increase(hybernate_target_unavailable_total[10m]) > 0` | warning |
| HybernatePVCRetentionExpiring | `hybernate_pvc_retention_remaining_seconds < 3600` | warning |
| HybernateRegimeChange | `increase(hybernate_prediction_regime_changes_total[1h]) > 0` | info |

### Example Alert Rule

```yaml title="alerts.yaml" linenums="1"
groups:
  - name: hybernate
    rules:
      - alert: HybernateReconcileErrors
        expr: rate(hybernate_reconcile_errors_total[5m]) > 0
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Hybernate reconciliation errors detected"
          description: "Controller {{ $labels.controller }} is experiencing reconcile errors."
```

## Health Checks

The operator exposes health endpoints:

```bash
# Liveness
curl http://localhost:8081/healthz

# Readiness
curl http://localhost:8081/readyz
```

Configure these in your Deployment's liveness and readiness probes (already set up in the default manifests).

## Operator Logs

For debugging, check the operator logs:

```bash
kubectl logs -n hybernate-system deployment/hybernate-controller-manager -f
```

Key log entries to watch for:

- `"pool scaled"`: scaling events with before/after counts
- `"phase transition"`: lifecycle state changes
- `"idle confirmed"`: idle detection results
- `"regime change"`: prediction engine pattern shifts
- `"drift detected"`: external replica changes
