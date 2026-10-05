# Configuration Reference

## Operator Flags

These flags are passed to the binary (`/manager`), which runs as the operator, or as the doorman with `--doorman`. The Helm chart sets them from its [values](helm-values.md); you only need them for your own manifests.

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-bind-address` | `0` (disabled) | Address for the metrics endpoint. Use `:8443` for HTTPS or `:8080` for HTTP. The chart and the kustomize manifests use `:8443` |
| `--metrics-secure` | `true` | Serve metrics over HTTPS, only to callers allowed to `get` `/metrics`, checked with a TokenReview and a SubjectAccessReview. `false` serves plain HTTP |
| `--metrics-cert-path` | | Directory containing the metrics server's TLS certificate. Without it, a self-signed certificate is generated |
| `--metrics-cert-name` | `tls.crt` | Metrics certificate file name |
| `--metrics-cert-key` | `tls.key` | Metrics key file name |
| `--enable-http2` | `false` | Allow HTTP/2 for the metrics server |
| `--health-probe-bind-address` | `:8081` | Address for the health and readiness probes |
| `--leader-elect` | `false` | Elect a leader, so only one replica reconciles. Ignored by the doorman |
| `--max-concurrent-reconciles` | `4` | How many ManagedWorkloads are reconciled at once, so one slow metrics or Prometheus query doesn't hold up wakes. At least 1 |
| `--timezone` | `UTC` | IANA time zone, such as `Europe/London`, whose hours and weekdays forecasts learn, so they follow daylight saving. The time zone database is built in |
| `--prometheus-url` | | Base URL of the Prometheus API for Prometheus activity queries, such as `http://prometheus.monitoring.svc:9090`; `http` or `https` with a host. Required only if workloads set `idlePolicy.activity.prometheus` |
| `--watch-namespaces` | every namespace | Comma-separated namespaces to work in, with a Role in each |
| `--protected-namespaces` | | Comma-separated name patterns, such as `prod-*`, of namespaces Hybernate never manages, as if labelled `hybernate.io/protected=true`, unless labelled `hybernate.io/allow-protected=true` |
| `--default-idle-after` | `1h` | Idle time before pausing, for workloads opted in with the label whose annotations and namespace don't set it. Must be above zero |
| `--default-cpu-threshold` | `10` | CPU percentage of requests that counts as active, for opted-in workloads that don't set it. 1 to 100 |
| `--default-dry-run` | `false` | Measure opted-in workloads without pausing, unless they or their namespace set `hybernate.io/dry-run` |
| `--doorman` | `false` | Run as the doorman instead of the operator. The doorman Deployment sets it |
| `--doorman-service` | `hybernate-doorman` | Name of the doorman's Service, which the operator routes paused workloads to. Empty disables waking on request |
| `--doorman-namespace` | `$POD_NAMESPACE`, else `hybernate-system` | Namespace of the doorman's Service |
| `--kubeconfig` | in-cluster | Path to a kubeconfig, only when running outside the cluster |
| `--zap-log-level` | `info` | Log level: `debug`, `info`, `error`, `panic`, or a whole number above 0 for more verbose debug levels |
| `--zap-encoder` | `json` | Log format: `json` or `console` |
| `--zap-devel` | `false` | Development defaults: console encoding, debug level, stack traces from warnings |
| `--zap-stacktrace-level` | `error` | Level from which stack traces are logged: `info`, `error`, or `panic` |
| `--zap-time-encoding` | RFC 3339 | Time format of log entries: `epoch`, `millis`, `nano`, `iso8601`, `rfc3339` or `rfc3339nano` |

The operator checks its flags at startup and exits with an error naming the flag when one is invalid: a `--protected-namespaces` pattern that isn't a valid glob, a `--prometheus-url` that isn't `http` or `https` with a host, a `--default-idle-after` that isn't above zero, a `--default-cpu-threshold` outside 1 to 100, a `--max-concurrent-reconciles` below 1, or a `--timezone` that isn't a known IANA zone.

## Endpoints

| Endpoint | Port | Description |
|----------|------|-------------|
| `/healthz` | 8081 | Liveness probe. Returns 200 while the process is running |
| `/readyz` | 8081 | Readiness probe. For the doorman, 200 once it has loaded its routes in the last 30 seconds, and failing while it shuts down |
| `/metrics` | 8443 in the chart | Prometheus metrics, HTTPS by default; see [Metrics](metrics.md) |

## Leader Election

For HA deployments with multiple replicas, enable leader election:

```
--leader-elect=true
```

The leader election ID is `479a98fc.hybernate.io`, held as a Lease in the operator's namespace. Only the leader runs reconciliation loops; standby replicas take over if the leader fails.

The doorman doesn't use leader election: every replica serves traffic.

## TLS Configuration

### Metrics

By default, metrics are served over HTTPS, and a scraper needs a token allowed to `get` the `/metrics` URL; the chart's `metrics.readerSubjects` binds one, see [Monitoring](../operations/monitoring.md). To use HTTP (not recommended for production):

```
--metrics-secure=false --metrics-bind-address=:8080
```

### Custom Certificates

For your own TLS certificate, instead of a self-signed one:

```
--metrics-cert-path=/certs/metrics
```

## Logging

The operator and the doorman log JSON by default, through `logr` (controller-runtime). Key fields in log entries:

| Field | Description |
|-------|-------------|
| `workload` | ManagedWorkload name |
| `namespace` | Workload namespace |
| `phase` | Current lifecycle phase |
| `from` / `to` | Phase transition |
| `reason` | Action reason |

For logs to read by eye, use `--zap-encoder=console` (the chart's `logEncoder: console`).
