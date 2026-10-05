# Helm Values Reference

Complete reference for the Hybernate Helm chart. Install with:

```bash
helm install hybernate oci://ghcr.io/okedeji/charts/hybernate --version 0.2.0 \
  --namespace hybernate-system --create-namespace
```

The chart is published only as an OCI artifact; there's no `helm repo` to add. Its version has no `v` (`0.2.0`); the image tags do (`v0.2.0`). To see all defaults:

```bash
helm show values oci://ghcr.io/okedeji/charts/hybernate --version 0.2.0
```

The chart validates values against its schema (`values.schema.json`), so a mistyped value, such as `logLevel: warn` or `defaults.cpuThreshold: 0`, fails the install or upgrade rather than the operator.

## Names

| Value | Default | Description |
|-------|---------|-------------|
| `nameOverride` | `""` | Replaces the chart name in resource names |
| `fullnameOverride` | `""` | Replaces the full name the chart's resources are named after |

The full name is the release name when it contains `hybernate`, and `<release>-hybernate` otherwise. For a release named `hybernate`, the operator Deployment and ServiceAccount are `hybernate`, and the doorman's are `hybernate-doorman`. The operator's pods are labelled `app.kubernetes.io/name: hybernate`, `app.kubernetes.io/instance: <release>` and `control-plane: controller-manager`; the doorman's the same, with `control-plane: doorman`.

## Image

| Value | Default | Description |
|-------|---------|-------------|
| `image.repository` | `ghcr.io/okedeji/hybernate` | Container image repository |
| `image.tag` | `""`, the chart's `appVersion`, such as `v0.2.0` | Image tag, for the operator and the doorman |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `imagePullSecrets` | `[]` | Private registry credentials |

## CRD

| Value | Default | Description |
|-------|---------|-------------|
| `crds.install` | `true` | Install the ManagedWorkload CRD, and upgrade it with each `helm upgrade` |

The CRD is a chart template with `helm.sh/resource-policy: keep`, so `helm uninstall` leaves it, and every ManagedWorkload, in place: deleting the CRD would delete them all. Set `crds.install: false` only if you manage the CRD yourself, for example from `config/crd` in the repository.

## Operator

| Value | Default | Description |
|-------|---------|-------------|
| `replicaCount` | `1` | Operator replicas. More than one needs `leaderElection.enabled`, so that only one acts at a time; the schema refuses more otherwise |
| `leaderElection.enabled` | `true` | Elect a leader through a Lease, so only one replica reconciles |
| `maxConcurrentReconciles` | `4` | How many ManagedWorkloads the operator reconciles at once, so one slow metrics or Prometheus query doesn't hold up waking another workload. Passed as `--max-concurrent-reconciles` |
| `timezone` | `UTC` | IANA time zone, such as `Europe/London`, whose hours and weekdays the [forecast](../concepts/forecasting.md#wall-clock-alignment) learns. Use your users' zone, so forecasts follow daylight saving. Passed as `--timezone` |
| `logLevel` | `info` | `debug`, `info`, `error`, `panic`, or a whole number above 0 for more verbose debug levels. For the operator and the doorman |
| `logEncoder` | `json` | `json`, or `console` for reading by eye. For the operator and the doorman |

## Resources

| Value | Default | Description |
|-------|---------|-------------|
| `resources.requests.cpu` | `10m` | CPU request |
| `resources.requests.memory` | `128Mi` | Memory request |
| `resources.limits.cpu` | `500m` | CPU limit |
| `resources.limits.memory` | `512Mi` | Memory limit |

The operator caches the cluster's Deployments, StatefulSets, Services, PVCs, HPAs, Nodes and Namespaces, or only the watched namespaces' with `watchNamespaces`, so its memory grows with the cluster. Raise the limit on clusters with many thousands of workloads. Go's garbage collector is given a soft limit of 90% of the memory limit, for the operator and the doorman alike, leaving the rest for memory outside its heap, so it works harder near the limit rather than letting either be OOM-killed. Setting `GOMEMLIMIT` in the pod overrides it.

## Namespaces

| Value | Default | Description |
|-------|---------|-------------|
| `watchNamespaces` | `[]` | Namespaces Hybernate works in. Empty means every namespace. Set, Hybernate gets a Role in each of them instead of a ClusterRole, so Kubernetes itself refuses it anything elsewhere; see [RBAC](#rbac). Passed as `--watch-namespaces`, to the doorman too |
| `protectedNamespaces` | `[]` | Name patterns, such as `prod-*`, of namespaces Hybernate never manages, as if labelled `hybernate.io/protected=true`, unless one is labelled `hybernate.io/allow-protected=true`. See [Protected namespaces](../guides/opt-in.md#protected-namespaces). Passed as `--protected-namespaces` |

```yaml title="values.yaml"
watchNamespaces: [preview-1, preview-2, staging]
protectedNamespaces: ["prod-*", production]
```

Wake on request works with `watchNamespaces`: the operator always reads the doorman's own EndpointSlices in the release namespace, through a `<fullname>-doorman-endpoints` Role there. A `dependsOn` pointing into a namespace that isn't watched is reported on the dependent, as `DependencyNotFound=True` with reason `DependencyNotVisible`, and never blocks it.

## Opt-In Defaults

Settings for workloads opted in with the `hybernate.io/managed` label, used when neither the workload nor its namespace sets the annotation. See [Opting In](../guides/opt-in.md#which-setting-wins).

| Value | Default | Description |
|-------|---------|-------------|
| `defaults.idleAfter` | `1h` | How long without activity before pausing; a Go duration above zero. Passed as `--default-idle-after` |
| `defaults.cpuThreshold` | `10` | CPU use, as a percentage of requests from 1 to 100, above which a workload counts as active. Passed as `--default-cpu-threshold` |
| `defaults.dryRun` | `false` | Measure without pausing. Set `true` to roll Hybernate out across a cluster measuring first. Passed as `--default-dry-run` |

## Prometheus Activity

| Value | Default | Description |
|-------|---------|-------------|
| `prometheus.url` | `""` | Base URL of the Prometheus API used for Prometheus activity queries, such as `http://prometheus.monitoring.svc:9090`. Passed to the operator as `--prometheus-url`. Leave empty if no workload sets `idlePolicy.activity.prometheus` |

## Metrics

| Value | Default | Description |
|-------|---------|-------------|
| `metrics.enabled` | `true` | Expose Prometheus metrics, from the operator and the doorman |
| `metrics.port` | `8443` | Metrics endpoint port |
| `metrics.secure` | `true` | Serve metrics over HTTPS, only to callers allowed to `get` `/metrics`. `false` serves plain HTTP to anyone who can reach the port |
| `metrics.readerSubjects` | `[]` | Subjects, such as your Prometheus's ServiceAccount, to bind to the `<fullname>-metrics-reader` ClusterRole, which may get `/metrics` |

With `metrics.secure`, a scraper must authenticate with a token that's allowed to get `/metrics`. The chart creates the `<fullname>-metrics-reader` ClusterRole for that; bind your Prometheus to it with `readerSubjects`, or with a binding of your own:

```yaml title="values.yaml"
metrics:
  readerSubjects:
    - kind: ServiceAccount
      name: prometheus-k8s
      namespace: monitoring
```

It also creates ClusterRoleBindings to `system:auth-delegator` for the operator and the doorman (`<fullname>-auth-delegator`, `<fullname>-doorman-auth-delegator`), which they need to check those tokens.

### ServiceMonitor

| Value | Default | Description |
|-------|---------|-------------|
| `metrics.serviceMonitor.enabled` | `false` | Create Prometheus Operator ServiceMonitors for the operator and the doorman |
| `metrics.serviceMonitor.interval` | `30s` | Scrape interval |
| `metrics.serviceMonitor.additionalLabels` | `{}` | Extra labels on the ServiceMonitors |

### PrometheusRule

| Value | Default | Description |
|-------|---------|-------------|
| `metrics.prometheusRule.enabled` | `false` | Create a PrometheusRule with the alerts in [Monitoring](../operations/monitoring.md#alerting-rules) |
| `metrics.prometheusRule.additionalLabels` | `{}` | Extra labels on the PrometheusRule |

## Doorman

The doorman holds requests to paused workloads and wakes them. See [Wake on Request](../concepts/wake-on-request.md).

| Value | Default | Description |
|-------|---------|-------------|
| `doorman.enabled` | `true` | Deploy the doorman. When `false`, requests to paused workloads fail |
| `doorman.replicaCount` | `2` | Doorman replicas. Every replica serves traffic |
| `doorman.podDisruptionBudget.enabled` | `true` | A PodDisruptionBudget keeping one doorman serving through node drains, when `replicaCount` is above 1 |
| `doorman.nodeSelector` | `{}` | Node selector; empty uses the operator's `nodeSelector` |
| `doorman.tolerations` | `[]` | Tolerations; empty uses the operator's `tolerations` |
| `doorman.affinity` | `{}` | Affinity; empty uses the operator's `affinity` |
| `doorman.topologySpreadConstraints` | `[]` | Topology spread; empty spreads the replicas across nodes and zones where it can |
| `doorman.resources.requests.cpu` | `10m` | CPU request |
| `doorman.resources.requests.memory` | `64Mi` | Memory request |
| `doorman.resources.limits.cpu` | `500m` | CPU limit |
| `doorman.resources.limits.memory` | `256Mi` | Memory limit |

The doorman uses the operator image, the `metrics.*` settings, `logLevel` and `logEncoder`. It caches ManagedWorkloads and the EndpointSlices the EndpointSlice controller writes in the watched namespaces, cut down to what it reads: about 300 bytes per pod and 700 per Service. Held connections take up to 72Mi more under a flood. The default 256Mi limit covers a cluster of some 100,000 pods; raise it beyond that. On Kubernetes 1.30 and later it sleeps 5 seconds before shutting down, so kube-proxy and load balancers stop sending it new connections first.

## Network Policy

| Value | Default | Description |
|-------|---------|-------------|
| `networkPolicy.enabled` | `false` | Create NetworkPolicies for the operator and the doorman |

When enabled, the operator's and the doorman's metrics port admits only namespaces labelled `metrics: enabled`, and the doorman's routing ports (TCP 20000-29999) admit every source, since every client of a paused workload's Services is sent to them. To narrow who can reach the doorman, see [Restricting who can reach the doorman](../concepts/wake-on-request.md#restricting-who-can-reach-the-doorman).

## Service Account

| Value | Default | Description |
|-------|---------|-------------|
| `serviceAccount.create` | `true` | Create the operator's ServiceAccount |
| `serviceAccount.name` | `""` | Name override (defaults to the full name) |

The doorman always gets its own ServiceAccount, `<fullname>-doorman`.

## Pod Scheduling

| Value | Default | Description |
|-------|---------|-------------|
| `nodeSelector` | `{}` | Node selector constraints, for the operator, and for the doorman unless `doorman.nodeSelector` is set |
| `tolerations` | `[]` | Pod tolerations, likewise |
| `affinity` | `{}` | Pod affinity/anti-affinity rules, likewise |

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

| Pod | Probe | Path | Port | Initial Delay | Period |
|-----|-------|------|------|--------------|--------|
| Operator | Liveness | `/healthz` | 8081 | 15s | 20s |
| Operator | Readiness | `/readyz` | 8081 | 5s | 10s |
| Doorman | Liveness | `/healthz` | 8081 | 15s | 20s |
| Doorman | Readiness | `/readyz` | 8081 | 2s | 5s |

## RBAC

By default the chart creates a ClusterRole, `<fullname>-manager`, with permissions to:

- Manage `ManagedWorkload` CRs, including creating them for labelled workloads
- Read Namespaces, for the `hybernate.io/managed` label and settings annotations on them
- Read Deployments and StatefulSets, and scale them through the scale subresource. Hybernate can't delete or otherwise change them
- Read PersistentVolumeClaims, to price the storage a paused workload keeps
- Read HorizontalPodAutoscalers, and read and annotate KEDA ScaledObjects, to pause a KEDA workload through KEDA
- Read pod metrics from metrics-server
- Read pods, to price what pausing a workload frees, sidecars included. Pods are read when needed rather than watched, so they aren't cached
- Read Nodes' metadata, to price workloads at the list price of the instance types they run on. Only names and labels are cached
- Get ConfigMaps, one at a time and uncached, to [learn dependencies](../concepts/dependencies.md#learned-dependencies) from the ones a workload's environment references. It can't list or change them
- Create Events for user-facing status updates
- Read Services, and manage the EndpointSlices that route paused workloads to the doorman

The doorman has its own ServiceAccount and ClusterRole, `<fullname>-doorman`: it reads ManagedWorkloads and EndpointSlices, patches ManagedWorkloads to wake them, and creates Events.

With `watchNamespaces` set, both get a Role and RoleBinding in each of those namespaces instead. The operator keeps one ClusterRole, to read Namespaces and Nodes, which only a ClusterRole can grant, and gets a `<fullname>-doorman-endpoints` Role in the release namespace to read the doorman's EndpointSlices. Workloads in other namespaces can't be read, scaled, or opted in, and dependencies on them aren't held or woken. With `metrics.secure`, both ServiceAccounts also keep their cluster-wide `system:auth-delegator` bindings, which let them check scrapers' tokens and nothing else.

If leader election is enabled, a `<fullname>-leader-election` Role in the release namespace grants Leases (`get`, `create`, `update`) and Events.

What all of this reads, writes, and sends is listed in [Data and Access](data-and-access.md).

## Example: Production Configuration

```yaml title="values-production.yaml" linenums="1"
replicaCount: 2

leaderElection:
  enabled: true

timezone: Europe/London

resources:
  requests:
    cpu: 100m
    memory: 256Mi
  limits:
    cpu: "1"
    memory: 1Gi

metrics:
  enabled: true
  secure: true
  readerSubjects:
    - kind: ServiceAccount
      name: prometheus-k8s
      namespace: monitoring
  serviceMonitor:
    enabled: true
    interval: 15s
  prometheusRule:
    enabled: true

protectedNamespaces: ["prod-*"]

affinity:
  podAntiAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        podAffinityTerm:
          labelSelector:
            matchLabels:
              app.kubernetes.io/name: hybernate
              control-plane: controller-manager
          topologyKey: kubernetes.io/hostname
```

`affinity` applies to the doorman too unless `doorman.affinity` is set; the doorman's replicas are spread across nodes by default.

```bash
helm install hybernate oci://ghcr.io/okedeji/charts/hybernate \
  --version 0.2.0 \
  -f values-production.yaml \
  -n hybernate-system --create-namespace
```
