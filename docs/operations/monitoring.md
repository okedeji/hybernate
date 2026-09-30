# Monitoring

Hybernate ships with Prometheus metrics for operator health and a set of alerting rules.

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
# Is the operator acting on workloads?
rate(hybernate_lifecycle_transitions_total[1h])

# Are there errors?
rate(hybernate_reconcile_errors_total[5m]) > 0
```

**Per-workload health:**

```promql
# Is the prediction engine learning?
hybernate_prediction_confidence_percent{season="daily"}

# Are workloads cycling too fast?
rate(hybernate_lifecycle_transitions_total[1h])
```

## Alerting Rules

The Helm chart creates these rules when `metrics.prometheusRule.enabled` is `true`; the same rules are in `config/prometheus/alerts.yaml` for kustomize installs.

| Alert | Fires when | Severity |
|-------|-----------|----------|
| `HybernateReconcileErrorsHigh` | Reconcile errors exceed 0.1/sec for 10 minutes | critical |
| `HybernateDown` | No healthy operator target is scraped for 5 minutes | critical |
| `HybernateWorkloadStuck` | A workload stays in `Pausing`, `Resuming`, or `Destroying` for 15 minutes | warning |
| `HybernateTargetUnavailable` | A ManagedWorkload's target is missing more than 3 times in an hour | warning |
| `HybernatePVCRetentionExpiring` | A destroyed workload's PVCs will be deleted within 24 hours | warning |

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

- `"phase transition"`: lifecycle state changes
- `"idle confirmed"`: idle detection results
- `"regime change"`: prediction engine pattern shifts
- `"paused workload scaled externally"`: someone scaled up a paused workload
