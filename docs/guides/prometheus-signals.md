# Prometheus Activity Guide

CPU tells Hybernate whether a workload is doing work. A Prometheus query tells it whether anyone is *using* it: requests through an ingress, open connections, messages arriving on a queue. Add queries as activity sources, and any result above zero keeps the workload awake.

## How It Works

On every check (about once a minute while the workload is awake), Hybernate runs each query in turn as an instant query against the Prometheus API (`/api/v1/query`), stopping at the first that shows activity. Each query has 2 seconds to answer, and all of a workload's queries 5 seconds together.

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

Set the Prometheus URL with the `prometheus.url` Helm value, which passes `--prometheus-url` to the operator. Pass the chart version you have installed as `--version`, which `helm list -n hybernate-system` shows, or Helm upgrades to the newest chart too:

```bash
helm upgrade hybernate oci://ghcr.io/okedeji/charts/hybernate --version X.Y.Z \
  --namespace hybernate-system --reuse-values \
  --set prometheus.url=http://prometheus.monitoring.svc.cluster.local:9090
```

The URL is the base of the Prometheus HTTP API, `http` or `https`; the operator refuses to start with any other. Hybernate appends `/api/v1/query` and keeps any path prefix, so endpoints like `https://mimir.example.com/prometheus` work as-is. It sends no credentials or headers, so a Prometheus that needs them must be reached through a proxy that adds them.

## When a Query Can't Be Evaluated

If a query fails, Hybernate can't see that activity, so it doesn't pause the workload. The `PrometheusAvailable` condition says why:

| Reason | Meaning |
|--------|---------|
| `EndpointNotConfigured` | Queries are configured but the operator has no `--prometheus-url` |
| `QueryFailed` | Prometheus returned an error, didn't answer a query within 2 seconds or all of the workload's within 5, or returned a range vector or an oversized result. The condition message has the details |

```bash
kubectl get managedworkload my-api -n staging \
  -o jsonpath='{.status.conditions[?(@.type=="PrometheusAvailable")]}'
```

The condition only appears on workloads that configure Prometheus queries.

A query that answers but matches no series at all looks just like a quiet workload, and that's also what a misspelt metric or label name gives. Hybernate can't tell the two apart, so the workload pauses on its other sources as usual, but a query that has matched no series since it was first evaluated is pointed out: `PrometheusQueryEmpty=True`, reason `NoSeries`, with the queries in its message, and a `PrometheusQueryEmpty` warning event when it begins. It clears once the query matches a series, even one at zero. A counter that's only created on the first request, as many client libraries' labelled counters are, also matches nothing until then; once it exists the condition clears. What a query has matched is remembered in memory, so after an operator restart a query is checked again from its first evaluation.

When Prometheus itself fails, by not answering, answering with a server error or `429`, or timing out on three queries in a row, the operator doesn't ask it again for a minute: every workload's queries fail at once, as `QueryFailed` with `prometheus failed recently, not asked again yet` and the original error, so none waits out a timeout and holds up the wakes queued behind it. A query Prometheus rejects, such as one that doesn't parse, fails only for the workloads that use it. Time that a query can't be evaluated isn't counted as idle: Prometheus out for longer than a couple of checks restarts the clock (`lastActivitySource: unobserved`), so a workload is never paused because Prometheus was down. A query that times out while other queries are answered fails only for the workloads that use it, so one tenant's slow query doesn't hold off everyone's. Still keep queries cheap: aggregate, and use recording rules for anything heavy.

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
