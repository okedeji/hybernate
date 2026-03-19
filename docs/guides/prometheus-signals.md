# Prometheus Signals Guide

Prometheus signals let you add application-level checks to idle detection and scale-down guards. Instead of relying on CPU alone, you can query actual business metrics.

## How Prometheus Signals Work

Hybernate sends an instant query to the Prometheus API (`/api/v1/query`) with a 5-second timeout.

- **Non-zero, non-empty result**: signal **confirms** the action
- **Zero value or empty result**: signal **denies** the action

The key insight: **write your PromQL so that a non-zero result means "yes, proceed with this action."**

## Configuration

Prometheus signals appear in two places:

### Idle Detection Signals

```yaml title="managedworkload.yaml" linenums="1"
spec:
  idlePolicy:
    signals:
      - source: prometheus
        promQL: 'rate(http_requests_total{service="my-api"}[10m]) == 0'
```

The query above returns `1` (non-zero) when the request rate is zero, confirming idle.

### Scale-Down Guards

```yaml title="managedworkload.yaml" linenums="1"
spec:
  scalePolicy:
    down:
      guard:
        - source: prometheus
          promQL: 'sum(active_connections{service="my-api"}) < 10'
```

The query returns `1` when active connections are below 10, confirming it's safe to scale down.

## Prometheus Endpoint

The operator needs to know where your Prometheus instance lives. This is configured via the `PROMETHEUS_ENDPOINT` environment variable on the operator deployment:

```yaml title="deployment.yaml" linenums="1"
env:
  - name: PROMETHEUS_ENDPOINT
    value: "http://prometheus.monitoring.svc.cluster.local:9090"
```

## Example Signals

### Idle Detection

| Use Case | PromQL | Logic |
|----------|--------|-------|
| No HTTP traffic | `rate(http_requests_total{service="api"}[10m]) == 0` | Confirms idle when request rate is zero |
| No WebSocket connections | `sum(websocket_connections{service="api"}) == 0` | Confirms idle when no connections |
| No queue messages | `sum(rabbitmq_queue_messages{queue="tasks"}) == 0` | Confirms idle when queue is empty |
| No active sessions | `sum(active_sessions{app="dashboard"}) == 0` | Confirms idle when no users |
| No recent logins | `increase(login_total{app="auth"}[1h]) == 0` | Confirms idle when no logins in the last hour |
| No Kafka consumer lag | `sum(kafka_consumer_lag{group="worker"}) == 0` | Confirms idle when consumer is caught up |

### Scale-Down Guards

| Use Case | PromQL | Logic |
|----------|--------|-------|
| Low connection count | `sum(active_connections{service="api"}) < 10` | Safe to scale when connections are low |
| No in-flight jobs | `sum(in_flight_jobs{service="worker"}) == 0` | Safe when all jobs are complete |
| Low memory pressure | `container_memory_working_set_bytes{pod=~"api-.*"} / container_spec_memory_limit_bytes{pod=~"api-.*"} < 0.7` | Safe when memory is below 70% |
| Low request latency | `histogram_quantile(0.99, rate(request_duration_seconds_bucket{service="api"}[5m])) < 0.5` | Safe when p99 latency is under 500ms |

## Writing Good Signals

### Use comparison operators for idle signals

Idle signals should evaluate to a boolean. Use `== 0` for "is this metric absent":

```yaml title="managedworkload.yaml" linenums="1"
# Good: Returns 1 (confirms idle) when rate is zero
promQL: 'rate(http_requests_total{service="api"}[10m]) == 0'

# Bad: Returns the rate value, which is zero when idle (denies idle!)
promQL: 'rate(http_requests_total{service="api"}[10m])'
```

### Use appropriate time ranges

Too short: noisy, may trigger on brief pauses between requests.
Too long: slow to react, may delay idle detection.

```yaml title="managedworkload.yaml" linenums="1"
# Good: 10-minute window smooths out brief gaps
promQL: 'rate(http_requests_total{service="api"}[10m]) == 0'

# Too short: 1-minute window may see zero rate between bursts
promQL: 'rate(http_requests_total{service="api"}[1m]) == 0'
```

### Filter by the specific workload

Include labels that identify your workload to avoid matching other services:

```yaml title="managedworkload.yaml" linenums="1"
# Good: specific to the service
promQL: 'sum(active_connections{service="my-api", namespace="staging"}) < 10'

# Bad: matches all services
promQL: 'sum(active_connections) < 10'
```

## Debugging Signals

Test your PromQL query directly against Prometheus first:

```bash
curl -s 'http://prometheus:9090/api/v1/query?query=rate(http_requests_total{service="api"}[10m])==0'
```

Check operator events for signal evaluation results:

```bash
kubectl describe managedworkload my-api -n staging
```

Events will show which signals confirmed or denied, helping you tune your queries.
