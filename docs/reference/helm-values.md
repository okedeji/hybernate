# Helm Values Reference

Complete reference for the Hybernate Helm chart. Install with:

```bash
helm install hybernate oci://ghcr.io/okedeji/charts/hybernate --version v0.1.7
```

To see all defaults:

```bash
helm show values oci://ghcr.io/okedeji/charts/hybernate --version v0.1.7
```

## Image

| Value | Default | Description |
|-------|---------|-------------|
| `image.repository` | `ghcr.io/okedeji/hybernate` | Container image repository |
| `image.tag` | Chart appVersion | Image tag |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `imagePullSecrets` | `[]` | Private registry credentials |

## Deployment

| Value | Default | Description |
|-------|---------|-------------|
| `replicaCount` | `1` | Number of operator pod replicas |
| `logLevel` | `info` | Zap log level (`debug`, `info`, `warn`, `error`) |

## Leader Election

| Value | Default | Description |
|-------|---------|-------------|
| `leaderElection.enabled` | `true` | Enable leader election for HA deployments |

When running multiple replicas, leader election ensures only one instance runs reconciliation loops. Uses Kubernetes Leases for coordination.

## Resources

| Value | Default | Description |
|-------|---------|-------------|
| `resources.requests.cpu` | `10m` | CPU request |
| `resources.requests.memory` | `64Mi` | Memory request |
| `resources.limits.cpu` | `500m` | CPU limit |
| `resources.limits.memory` | `128Mi` | Memory limit |

Memory usage scales with the number of ManagedWorkloads. Each workload's forecast engine state is ~10KB. For 1000 workloads, expect ~10MB of additional memory.

## Prometheus Activity

| Value | Default | Description |
|-------|---------|-------------|
| `prometheus.url` | `""` | Base URL of the Prometheus API used for Prometheus activity queries. Passed to the operator as `--prometheus-url`. Leave empty if no workload sets `idlePolicy.activity.prometheus`. |

## Metrics

| Value | Default | Description |
|-------|---------|-------------|
| `metrics.enabled` | `true` | Expose Prometheus metrics |
| `metrics.port` | `8443` | Metrics endpoint port |
| `metrics.secure` | `true` | Serve metrics over HTTPS with authentication |

When `metrics.secure` is `true`, the chart creates a `ClusterRoleBinding` to `system:auth-delegator` for token authentication on the metrics endpoint.

### ServiceMonitor

| Value | Default | Description |
|-------|---------|-------------|
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus ServiceMonitor CR |
| `metrics.serviceMonitor.interval` | `30s` | Scrape interval |
| `metrics.serviceMonitor.additionalLabels` | `{}` | Extra labels on the ServiceMonitor |

### PrometheusRule

| Value | Default | Description |
|-------|---------|-------------|
| `metrics.prometheusRule.enabled` | `false` | Create a PrometheusRule CR with predefined alerts |
| `metrics.prometheusRule.additionalLabels` | `{}` | Extra labels on the PrometheusRule |

The PrometheusRule includes alerts for reconciliation errors, operator downtime, workloads stuck in a transition, missing targets, and PVC retention expiry.

## Doorman

The doorman holds requests to paused workloads and wakes them. See [Wake on Request](../concepts/wake-on-request.md).

| Value | Default | Description |
|-------|---------|-------------|
| `doorman.enabled` | `true` | Deploy the doorman. When `false`, requests to paused workloads fail |
| `doorman.replicaCount` | `2` | Doorman replicas. Every replica serves traffic |
| `doorman.resources.requests.cpu` | `10m` | CPU request |
| `doorman.resources.requests.memory` | `32Mi` | Memory request |
| `doorman.resources.limits.cpu` | `500m` | CPU limit |
| `doorman.resources.limits.memory` | `128Mi` | Memory limit |

The doorman uses the operator image and the `metrics.*` settings. With `metrics.serviceMonitor.enabled`, it gets its own ServiceMonitor.

## Network Policy

| Value | Default | Description |
|-------|---------|-------------|
| `networkPolicy.enabled` | `false` | Create a NetworkPolicy for metrics access |

When enabled, allows ingress on the metrics port only from namespaces labeled `metrics: enabled`.

## Service Account

| Value | Default | Description |
|-------|---------|-------------|
| `serviceAccount.create` | `true` | Create a service account |
| `serviceAccount.name` | `""` | Name override (defaults to release fullname) |

## Pod Scheduling

| Value | Default | Description |
|-------|---------|-------------|
| `nodeSelector` | `{}` | Node selector constraints |
| `tolerations` | `[]` | Pod tolerations |
| `affinity` | `{}` | Pod affinity/anti-affinity rules |

## Security

The chart enforces a strict security posture by default:

**Pod level:**

- `runAsNonRoot: true`
- `seccompProfile.type: RuntimeDefault`

**Container level:**

- `readOnlyRootFilesystem: true`
- `allowPrivilegeEscalation: false`
- `capabilities.drop: ["ALL"]`

These are not configurable via values. To override, use Helm post-rendering or Kustomize overlays.

## Health Probes

| Probe | Path | Port | Initial Delay | Period |
|-------|------|------|--------------|--------|
| Liveness | `/healthz` | 8081 | 15s | 20s |
| Readiness | `/readyz` | 8081 | 5s | 10s |

## RBAC

The chart creates a ClusterRole with permissions to:

- Manage `ManagedWorkload` and `WorkloadPolicy` CRs
- Read and scale Deployments and StatefulSets
- Read PersistentVolumeClaims (for retention cleanup)
- Read pod metrics from metrics-server
- Create Events for user-facing status updates
- Read Services, and manage the EndpointSlices that route paused workloads to the doorman

The doorman has its own ServiceAccount and ClusterRole: it reads ManagedWorkloads and EndpointSlices, patches ManagedWorkloads to wake them, and creates Events.

If leader election is enabled, a namespaced Role is created for Lease and ConfigMap access.

## Example: Production Configuration

```yaml title="values-production.yaml" linenums="1"
replicaCount: 2

leaderElection:
  enabled: true

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: "1"
    memory: 256Mi

metrics:
  enabled: true
  secure: true
  serviceMonitor:
    enabled: true
    interval: 15s
  prometheusRule:
    enabled: true

logLevel: info

affinity:
  podAntiAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        podAffinityTerm:
          labelSelector:
            matchLabels:
              app.kubernetes.io/name: hybernate
          topologyKey: kubernetes.io/hostname
```

```bash
helm install hybernate oci://ghcr.io/okedeji/charts/hybernate \
  --version v0.1.7 \
  -f values-production.yaml \
  -n hybernate-system --create-namespace
```
