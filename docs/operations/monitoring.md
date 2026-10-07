# Monitoring

Hybernate ships with Prometheus metrics for the operator and the doorman, and a set of alerting rules. See the [Metrics Reference](../reference/metrics.md) for every metric.

## Prometheus

### Scraping

Metrics are served over HTTPS, and only to a scraper whose token is allowed to `get` the `/metrics` URL.

**With Helm**, turn on the ServiceMonitors (one for the operator, one for the doorman) and bind your Prometheus's ServiceAccount to the chart's `<fullname>-metrics-reader` ClusterRole:

```yaml title="values.yaml"
metrics:
  serviceMonitor:
    enabled: true
  prometheusRule:
    enabled: true
  readerSubjects:
    - kind: ServiceAccount
      name: prometheus-k8s
      namespace: monitoring
```

**With the kustomize manifests**, apply the ServiceMonitors and the alerting rules into `hybernate-system`:

```bash
kubectl apply -k config/prometheus
```

and bind your Prometheus's ServiceAccount to the `hybernate-metrics-reader` ClusterRole yourself:

```bash
kubectl create clusterrolebinding hybernate-metrics-reader \
  --clusterrole=hybernate-metrics-reader --serviceaccount=monitoring:prometheus-k8s
```

Without that binding, the targets show as down with `401` or `403`, and the `HybernateDown` alert fires. With `metrics.secure: false` in the chart, metrics are plain HTTP and need no binding, but anyone who can reach the port can read them.

### Key Metrics to Watch

**Cluster health dashboard:**

```promql
# Is the operator acting on workloads?
rate(hybernate_lifecycle_transitions_total[1h])

# Are there errors?
rate(hybernate_reconcile_errors_total[5m]) > 0

# Are requests to paused workloads waking them?
sum by (result) (rate(hybernate_doorman_wakes_total[1h]))
```

**Per-workload health:**

```promql
# Is the prediction engine learning?
hybernate_prediction_confidence_percent{season="daily"}

# Are workloads cycling too fast?
rate(hybernate_lifecycle_transitions_total[1h])
```

## Alerting Rules

The Helm chart creates these rules when `metrics.prometheusRule.enabled` is `true`; `kubectl apply -k config/prometheus` creates the same rules for kustomize installs.

| Alert | Fires when | Severity |
|-------|-----------|----------|
| `HybernateReconcileErrorsHigh` | Reconcile errors exceed 0.1/sec for 10 minutes | critical |
| `HybernateDown` | No healthy operator target has been scraped for 5 minutes, so nothing is paused or woken | critical |
| `HybernateDoormanDown` | No healthy doorman target has been scraped for 5 minutes, so requests to paused workloads fail instead of waking them. Only with the doorman enabled | critical |
| `HybernateWorkloadStuck` | A workload stays in `Pausing` or `Resuming` for 15 minutes | warning |
| `HybernateTargetUnavailable` | A ManagedWorkload's target goes missing more than 3 times in an hour | warning |
| `HybernateDoormanWakesFailing` | More than 3 requests to one paused workload timed out or failed (`result` `timeout` or `error`) in 15 minutes, for 5 minutes | warning |

`HybernateDown` and `HybernateDoormanDown` match the scrape job of the operator's and the doorman's metrics Service, so they need the ServiceMonitors above.

## Health Checks

The operator and the doorman expose health endpoints on port 8081, which the chart and the kustomize manifests already use as liveness and readiness probes:

```bash
# Liveness
curl http://localhost:8081/healthz

# Readiness
curl http://localhost:8081/readyz
```

## Operator Logs

Logs are JSON by default. For debugging, check the operator logs; the label selectors work for both Helm and kustomize installs:

```bash
kubectl logs -n hybernate-system -l control-plane=controller-manager -f
kubectl logs -n hybernate-system -l control-plane=doorman -f
```

The Deployments themselves are `hybernate` and `hybernate-doorman` for a Helm release named `hybernate`, and `hybernate-controller-manager` and `hybernate-doorman` with kustomize.

Log entries to look for:

- `"phase transition"`: lifecycle state changes, with `from`, `to` and `reason`
- `"paused workload scaled up outside Hybernate"`: someone scaled up a paused workload
- `"managing workload from its label"`: the opt-in controller created a ManagedWorkload
- `"routing requests through the doorman"`: an error routing a paused workload's Services to the doorman
- `"ignoring malformed activity annotation"`: an activity annotation that isn't an RFC 3339 time

Forecast regime changes, idle detection and wakes are recorded as [events](../concepts/lifecycle.md#events) on the ManagedWorkload rather than logged.
