# Prometheus Activity Guide

CPU tells Hybernate whether a workload is doing work. A Prometheus query tells it whether anyone is *using* it: requests through an ingress, open connections, messages arriving on a queue. Add queries as activity sources, and any result above zero keeps the workload awake.

## How It Works

On every check (about once a minute while the workload is awake), Hybernate runs each query as an instant query against the Prometheus API (`/api/v1/query`), with a 5-second timeout.

- **Any series above zero**: activity. The [activity clock](../concepts/idle-detection.md) resets. With a series per pod, one busy pod is enough.
- **Zero, or an empty result**: no activity from this source. The clock keeps running.
- **`NaN` or infinite values** are ignored, as no evidence either way: `NaN` is what a ratio of two idle counters gives (0/0). A result with nothing else in it is treated like an empty one: it doesn't confirm activity, and the other sources decide.

The query must return an instant vector or a scalar; a range vector (`metric[5m]` on its own) fails as `QueryFailed`. A response larger than 4 MiB, from a query that returns a series per pod across the cluster, is refused too: aggregate with `sum` or `max`.

Write the query so that it measures activity, not idleness: "how many requests per second", not "are there no requests".

## Configuration

```yaml title="managedworkload.yaml" linenums="1"
spec:
  idlePolicy:
    idleAfter: 1h
    activity:
      prometheus:
        - promQL: 'sum(rate(nginx_ingress_controller_requests{exported_service="my-api"}[5m]))'
```

Queries are combined with the other activity sources: CPU, deploys, and activity annotations. Any one of them is enough to keep the workload awake.

Prometheus queries can't be set with an annotation. On a ManagedWorkload created from the `hybernate.io/managed` label, add them to its `spec.idlePolicy.activity.prometheus`: the opt-in controller keeps them when it updates the rest from the annotations.

## Prometheus Endpoint

Set the Prometheus URL with the `prometheus.url` Helm value, which passes `--prometheus-url` to the operator:

```bash
helm upgrade hybernate oci://ghcr.io/okedeji/charts/hybernate \
  --namespace hybernate-system --reuse-values \
  --set prometheus.url=http://prometheus.monitoring.svc.cluster.local:9090
```

The URL is the base of the Prometheus HTTP API, `http` or `https`; the operator refuses to start with any other. Hybernate appends `/api/v1/query` and keeps any path prefix, so endpoints like `https://mimir.example.com/prometheus` work as-is. It sends no credentials or headers, so a Prometheus that needs them must be reached through a proxy that adds them.

## When a Query Can't Be Evaluated

If a query fails, Hybernate can't see that activity, so it doesn't pause the workload. The `PrometheusAvailable` condition says why:

| Reason | Meaning |
|--------|---------|
| `EndpointNotConfigured` | Queries are configured but the operator has no `--prometheus-url` |
| `QueryFailed` | Prometheus returned an error, couldn't be reached within 5 seconds, or returned a range vector or an oversized result. The condition message has the details |

```bash
kubectl get managedworkload my-api -n staging \
  -o jsonpath='{.status.conditions[?(@.type=="PrometheusAvailable")]}'
```

The condition only appears on workloads that configure Prometheus queries.

## Example Queries

| Use case | PromQL |
|----------|--------|
| Requests through ingress-nginx | `sum(rate(nginx_ingress_controller_requests{exported_service="api"}[5m]))` |
| Requests seen by the app | `sum(rate(http_requests_total{service="api"}[5m]))` |
| Open WebSocket connections | `sum(websocket_connections{service="api"})` |
| Messages waiting on a queue | `sum(rabbitmq_queue_messages{queue="tasks"})` |
| Kafka consumer lag | `sum(kafka_consumer_lag{group="worker"})` |

## Writing Good Queries

### Use a rate window that matches how quiet "idle" is

`rate(...[5m])` stays above zero for about five minutes after the last request. That's usually what you want: a single request keeps the workload awake, and the clock starts once traffic has stopped.

### Exclude traffic that isn't people

Health checks, uptime monitors, and scrapers keep a workload awake if the query counts them. Filter them out by path or user agent where your metrics allow it:

```promql
sum(rate(nginx_ingress_controller_requests{exported_service="api", path!~"/healthz|/metrics"}[5m]))
```

### Test the query directly

```bash
curl -s 'http://prometheus:9090/api/v1/query' \
  --data-urlencode 'query=sum(rate(nginx_ingress_controller_requests{exported_service="api"}[5m]))'
```

Then check what kept the workload awake:

```bash
kubectl get managedworkload my-api -n staging -o jsonpath='{.status.activity.lastActivitySource}'
```

`prometheus` means a query was the most recent source of activity.
