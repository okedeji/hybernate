# Configuration Reference

## Operator Flags

These flags are passed to the operator binary (`manager`).

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-bind-address` | `0` (disabled) | Address for the metrics endpoint. Use `:8443` for HTTPS or `:8080` for HTTP. |
| `--metrics-secure` | `true` | Serve metrics over HTTPS with authentication. Set `false` for HTTP. |
| `--metrics-cert-path` | | Directory containing TLS cert for metrics server |
| `--metrics-cert-name` | `tls.crt` | Metrics certificate file name |
| `--metrics-cert-key` | `tls.key` | Metrics key file name |
| `--health-probe-bind-address` | `:8081` | Address for health and readiness probes |
| `--leader-elect` | `false` | Enable leader election for HA deployments |
| `--webhook-cert-path` | | Directory containing webhook TLS certificate |
| `--webhook-cert-name` | `tls.crt` | Webhook certificate file name |
| `--webhook-cert-key` | `tls.key` | Webhook key file name |
| `--enable-http2` | `false` | Allow HTTP/2 for metrics and webhook servers |
| `--zap-devel` | `true` | Development mode logging (human-readable) |
| `--zap-log-level` | `info` | Log level (`debug`, `info`, `error`) |
| `--zap-encoder` | `console` | Log format (`console` or `json`) |

## Environment Variables

| Variable | Description |
|----------|-------------|
| `PROMETHEUS_ENDPOINT` | URL of the Prometheus instance for PromQL signal queries (e.g., `http://prometheus.monitoring.svc.cluster.local:9090`) |

## Endpoints

| Endpoint | Port | Description |
|----------|------|-------------|
| `/healthz` | 8081 | Liveness probe. Returns 200 when the operator is running. |
| `/readyz` | 8081 | Readiness probe. Returns 200 when the operator is ready to reconcile. |
| `/metrics` | 8443 | Prometheus metrics (HTTPS by default) |

## Resource Requirements

Recommended resource requests/limits for the operator:

```yaml title="deployment.yaml" linenums="1"
resources:
  requests:
    cpu: 100m
    memory: 64Mi
  limits:
    cpu: 500m
    memory: 128Mi
```

The operator's memory usage scales with the number of ManagedWorkloads. Each workload's forecast engine state is ~10KB. For 1000 workloads, expect ~10MB of additional memory.

## Leader Election

For HA deployments with multiple replicas, enable leader election:

```
--leader-elect=true
```

The leader election ID is `479a98fc.hybernate.io`. Only the leader runs reconciliation loops; standby replicas take over if the leader fails.

## TLS Configuration

### Metrics

By default, metrics are served over HTTPS with Kubernetes authentication. To use HTTP (not recommended for production):

```
--metrics-secure=false --metrics-bind-address=:8080
```

### Custom Certificates

For custom TLS certificates (instead of auto-generated):

```
--metrics-cert-path=/certs/metrics
--webhook-cert-path=/certs/webhook
```

## Logging

The operator uses structured logging via `logr` (controller-runtime). Key fields in log entries:

| Field | Description |
|-------|-------------|
| `workload` | ManagedWorkload name |
| `namespace` | Workload namespace |
| `phase` | Current lifecycle phase |
| `from` / `to` | Phase transition |
| `reason` | Action reason |

### Production Logging

For production, use JSON encoding:

```
--zap-devel=false --zap-encoder=json --zap-log-level=info
```
