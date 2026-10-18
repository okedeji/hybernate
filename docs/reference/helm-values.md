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

The chart validates values against its schema (`values.schema.json`), so a mistyped value, such as `logLevel: warn` or `defaults.cpuThreshold: 0`, fails the install or upgrade rather than the operator, and so does a value the chart doesn't have, such as `watchNamespace` for `watchNamespaces`, which Helm would otherwise ignore. Only what's passed through to Kubernetes as it is, such as `resources`, `affinity`, `tolerations`, `nodeSelector` and pod annotations and labels, is left for Kubernetes to check. `timezone` is the exception: the schema can't list every IANA zone, so an unknown one is caught when the operator starts, which then exits with `invalid --timezone` and `unknown time zone` in its log, and the rollout doesn't finish.

## Names

| Value | Default | Description |
|-------|---------|-------------|
| `nameOverride` | `""` | Replaces the chart name in resource names |
| `fullnameOverride` | `""` | Replaces the full name the chart's resources are named after |

The full name is the release name when it contains `hybernate`, and `<release>-hybernate` otherwise, cut to 40 characters, so that every name built from it, the longest being `<fullname>-doorman-auth-delegator`, fits in 63. For a release named `hybernate`, the operator Deployment and ServiceAccount are `hybernate`, and the doorman's are `hybernate-doorman`. The operator's pods are labelled `app.kubernetes.io/name: hybernate`, `app.kubernetes.io/instance: <release>` and `control-plane: controller-manager`; the doorman's the same, with `control-plane: doorman`.

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

The CRD is a chart template with `helm.sh/resource-policy: keep`, so `helm uninstall` leaves it, and every ManagedWorkload, in place: deleting the CRD would delete them all. Argo CD ignores that policy, so the CRD also carries `argocd.argoproj.io/sync-options: Delete=false`, and deleting or pruning the Application leaves it too. Set `crds.install: false` only if you manage the CRD yourself, for example from `config/crd` in the repository.

## Operator

| Value | Default | Description |
|-------|---------|-------------|
| `replicaCount` | `1` | Operator replicas. More than one needs `leaderElection.enabled`, so that only one acts at a time; the schema refuses more otherwise |
| `leaderElection.enabled` | `true` | Elect a leader through a Lease, so only one replica reconciles. Without it, an upgrade stops the old operator before starting the new one (`strategy: Recreate`), so two never act at once |
| `maxConcurrentReconciles` | `8` | How many ManagedWorkloads the operator reconciles at once, so one slow metrics or Prometheus query doesn't hold up waking another workload. Passed as `--max-concurrent-reconciles` |
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

The operator caches the cluster's ManagedWorkloads, Deployments, StatefulSets, Services, PVCs, HPAs, Nodes and Namespaces, or only the watched namespaces' with `watchNamespaces`, and the EndpointSlices it writes and the doorman Service's, so its memory grows with the cluster. Nodes are cached as names and labels only, and other EndpointSlices, ConfigMaps, pods and KEDA ScaledObjects are read when needed, not cached. Raise the limit on clusters with many thousands of workloads. Go's garbage collector is given a soft limit of 90% of the memory limit, for the operator and the doorman alike, leaving the rest for memory outside its heap, so it works harder near the limit rather than letting either be OOM-killed. Setting `GOMEMLIMIT` in the pod overrides it.

## Namespaces

| Value | Default | Description |
|-------|---------|-------------|
| `watchNamespaces` | `[]` | Namespaces Hybernate works in. Empty means every namespace. Set, Hybernate gets a Role in each of them instead of a ClusterRole, so Kubernetes itself refuses it anything elsewhere; see [RBAC](#rbac). Passed as `--watch-namespaces`, to the doorman too |
| `protectedNamespaces` | `[]` | Name patterns, such as `prod-*`, of namespaces Hybernate never manages, as if labelled `hybernate.io/protected=true`, unless one is labelled `hybernate.io/allow-protected=true`. `kube-*`, and the operator's and the doorman's namespace, are always protected, besides these, with the same exception. See [Protected namespaces](../guides/opt-in.md#protected-namespaces). Passed as `--protected-namespaces` |

```yaml title="values.yaml"
watchNamespaces: [preview-1, preview-2, staging]
protectedNamespaces: ["prod-*", production]
```

Wake on request works with `watchNamespaces`: the operator always reads the doorman's own EndpointSlices in the release namespace, through a `<fullname>-doorman-endpoints` Role there. A `dependsOn` pointing into a namespace that isn't watched, and that allows the dependent's with `hybernate.io/dependents-from`, is reported on the dependent, as `DependencyNotFound=True` with reason `DependencyNotVisible`, and never blocks it.

An upgrade or rollback to a `watchNamespaces` that leaves out a namespace where the operator manages workloads, or that sets it where it was empty, is refused while they're there, since they'd stay paused with no operator: see [Narrowing watchNamespaces](../operations/upgrading.md#narrowing-watchnamespaces).

## Opt-In Defaults

Settings for workloads opted in with the `hybernate.io/managed` label, used when neither the workload nor its namespace sets the annotation. See [Opting In](../guides/opt-in.md#which-setting-wins).

| Value | Default | Description |
|-------|---------|-------------|
| `defaults.idleAfter` | `1h` | How long without activity before pausing; a Go duration of at least `1m`, such as `90s` or `1h30m`. Passed as `--default-idle-after` |
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
| `metrics.port` | `8443` | Metrics endpoint port, for the operator and the doorman. Not 8081, the health probes' port, nor 20000-29999, where the doorman listens for paused workloads' traffic |
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

With `metrics.enabled`, it also creates ClusterRoleBindings to `system:auth-delegator` for the operator and the doorman (`<fullname>-auth-delegator`, `<fullname>-doorman-auth-delegator`), which they need to check those tokens.

### ServiceMonitor

| Value | Default | Description |
|-------|---------|-------------|
| `metrics.serviceMonitor.enabled` | `false` | Create Prometheus Operator ServiceMonitors for the operator and the doorman |
| `metrics.serviceMonitor.interval` | `30s` | Scrape interval |
| `metrics.serviceMonitor.additionalLabels` | `{}` | Extra labels on the ServiceMonitors. Your Prometheus picks them up only if these match its `serviceMonitorSelector`: kube-prometheus-stack needs `release: <its release name>` |

The ServiceMonitors set `honorLabels: true`, so the metrics keep the workload's `namespace` label. kube-prometheus-stack's Prometheus runs as the `<its release name>-kube-prometheus-prometheus` ServiceAccount, the subject for `metrics.readerSubjects`. See [Monitoring](../operations/monitoring.md#scraping).

### PrometheusRule

| Value | Default | Description |
|-------|---------|-------------|
| `metrics.prometheusRule.enabled` | `false` | Create a PrometheusRule with the alerts in [Monitoring](../operations/monitoring.md#alerting-rules). `HybernateDown` and `HybernateDoormanDown`, which match the ServiceMonitors' scrape jobs, only with `metrics.serviceMonitor.enabled` |
| `metrics.prometheusRule.additionalLabels` | `{}` | Extra labels on the PrometheusRule. Your Prometheus loads it only if these match its `ruleSelector`: kube-prometheus-stack needs `release: <its release name>` |

## Doorman

The doorman holds requests to paused workloads and wakes them. See [Wake on Request](../concepts/wake-on-request.md).

| Value | Default | Description |
|-------|---------|-------------|
| `doorman.enabled` | `true` | Deploy the doorman. When `false`, requests to paused workloads fail |
| `doorman.replicaCount` | `2` | Doorman replicas. Every replica serves traffic |
| `doorman.portsPerWorkload` | `32` | The most doorman ports one workload may hold, one per routed Service port, so one tenant can't use up the 10,000 the cluster shares; 1 to 10000. A workload with more isn't routed: `WakeOnRequest=False`, reason `TooManyPorts`. Passed as `--doorman-ports-per-workload` |
| `doorman.portsPerNamespace` | `256` | The most doorman ports the workloads of one namespace may hold together; 1 to 10000. Passed as `--doorman-ports-per-namespace` |
| `doorman.healthCheckUserAgents` | `[]` | `User-Agent` prefixes, or names that follow `(compatible; ` in a browser-style one, of health checkers and scrapers whose `GET` and `HEAD` requests don't wake a paused workload, besides the built-in ones (`kube-probe`, Prometheus, load balancers' checks, `Blackbox Exporter/`, `Datadog/Synthetics`, `Amazon-Route53-Health-Check-Service`, `GoogleStackdriverMonitoring-UptimeChecks`, `UptimeRobot`, `Pingdom.com_bot`, `NewRelicPinger/`, `Uptime-Kuma/`, `Gatus/` and others), such as `["MyCorpMonitor/"]`. See [What wakes a workload](../concepts/wake-on-request.md#what-wakes-a-workload-and-what-doesnt) |
| `doorman.admissionPolicy.enabled` | `true` | A ValidatingAdmissionPolicy that lets the doorman's ServiceAccount change only the `hybernate.io/last-request` and `hybernate.io/last-request-from` annotations on ManagedWorkloads, though its role may patch them. See [Security](security.md#the-doorman-is-a-way-in-to-paused-workloads) |
| `doorman.podDisruptionBudget.enabled` | `true` | A PodDisruptionBudget keeping one doorman serving through node drains, when `replicaCount` is above 1 |
| `doorman.nodeSelector` | `{}` | Node selector; empty uses the operator's `nodeSelector` |
| `doorman.tolerations` | `[]` | Tolerations; empty uses the operator's `tolerations` |
| `doorman.affinity` | `{}` | Affinity; empty uses the operator's `affinity` |
| `doorman.topologySpreadConstraints` | `[]` | Topology spread; empty spreads the replicas across nodes and zones where it can |
| `doorman.podAnnotations` | `{}` | Pod annotations; empty uses the operator's `podAnnotations` |
| `doorman.podLabels` | `{}` | Pod labels; empty uses the operator's `podLabels` |
| `doorman.priorityClassName` | `""` | PriorityClass; empty uses the operator's `priorityClassName` |
| `doorman.extraEnv` | `[]` | Extra environment variables for the doorman. The operator's `extraEnv` doesn't apply to it |
| `doorman.resources.requests.cpu` | `10m` | CPU request |
| `doorman.resources.requests.memory` | `64Mi` | Memory request |
| `doorman.resources.limits.cpu` | `500m` | CPU limit |
| `doorman.resources.limits.memory` | `256Mi` | Memory limit |

The doorman uses the operator image, the `metrics.*` settings, `logLevel` and `logEncoder`. It caches ManagedWorkloads and the EndpointSlices the EndpointSlice controller writes in the watched namespaces, cut down to what it reads: about 300 bytes per pod and 700 per Service. Held connections take up to 72Mi more under a flood. The default 256Mi limit covers a cluster of some 100,000 pods; raise it beyond that. On Kubernetes 1.30 and later it sleeps 5 seconds before shutting down, so kube-proxy and load balancers stop sending it new connections first. A rollout waits 5 seconds after a new doorman pod is Ready (`minReadySeconds`) before stopping an old one, so the operator has moved paused workloads' routes to it.

## Network Policy

| Value | Default | Description |
|-------|---------|-------------|
| `networkPolicy.enabled` | `false` | Create NetworkPolicies for the operator and the doorman |
| `doorman.networkPolicy.ingressFrom` | `[]` | NetworkPolicy peers allowed to reach paused workloads through the doorman, such as your ingress controller's namespace. Empty leaves the doorman open to every caller. Needs `networkPolicy.enabled` |
| `doorman.networkPolicy.egress.enabled` | `false` | Create an egress NetworkPolicy allowing the doorman to connect only to `podCIDRs` and the API server |
| `doorman.networkPolicy.egress.podCIDRs` | `[]` | The cluster's pod CIDRs, in every IP family it uses. Required when enabled |
| `doorman.networkPolicy.egress.apiServer` | `[]` | CIDRs of the API server's endpoints (`kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes`), not the `kubernetes` Service's ClusterIP. Required when enabled |
| `doorman.networkPolicy.egress.apiServerPorts` | `[443, 6443]` | The API server endpoints' ports |

When enabled, the operator's and the doorman's metrics port admits only namespaces labelled `metrics: enabled`, and the doorman's routing ports (TCP 20000-29999) admit every source, since every client of a paused workload's Services is sent to them. To narrow who can reach the doorman, see [Restricting who can reach the doorman](../concepts/wake-on-request.md#restricting-who-can-reach-the-doorman).

The doorman's egress policy is separate from `networkPolicy.enabled`, and needs a CNI that enforces egress policies. The doorman needs no DNS: it reaches the API server by address, and pods by their endpoint addresses. If a routed workload uses `hostNetwork`, add your node CIDRs to `podCIDRs`.

On Cilium, a NetworkPolicy `ipBlock` may not match the API server, which Cilium identifies as the `kube-apiserver` entity rather than by address, so the egress policy can cut the doorman off from it: its pods then fail readiness and log API errors. Leave `doorman.networkPolicy.egress.enabled` off there, and write a CiliumNetworkPolicy instead that allows egress `toEntities: [kube-apiserver]` and `toCIDR` your pod CIDRs.

## Service Account

| Value | Default | Description |
|-------|---------|-------------|
| `serviceAccount.create` | `true` | Create the operator's ServiceAccount |
| `serviceAccount.name` | `""` | Name override (defaults to the full name) |

The doorman always gets its own ServiceAccount, `<fullname>-doorman`.

## Guards

| Value | Default | Description |
|-------|---------|-------------|
| `guard.uninstall` | `true` | Refuse `helm uninstall` while ManagedWorkloads are left, through a pre-delete hook Job |
| `guard.upgrade` | `true` | Refuse `helm upgrade` or `helm rollback` to a `watchNamespaces` that leaves out namespaces where the operator manages workloads, through a pre-upgrade and pre-rollback hook Job. Only with `watchNamespaces` set |
| `guard.nodeSelector` | `{}` | The hooks' node selector; empty uses the operator's `nodeSelector` |
| `guard.tolerations` | `[]` | Tolerations; empty uses the operator's `tolerations` |
| `guard.affinity` | `{}` | Affinity; empty uses the operator's `affinity` |
| `guard.priorityClassName` | `""` | PriorityClass; empty uses the operator's `priorityClassName` |
| `guard.podAnnotations` | `{}` | The hooks' pod annotations, over `sidecar.istio.io/inject: "false"` and `linkerd.io/inject: disabled` |
| `guard.podLabels` | `{}` | The hooks' pod labels, over `sidecar.istio.io/inject: "false"`. They can't replace the chart's own labels |
| `guard.resources` | 10m/64Mi requests, 100m/128Mi limits | The hooks' resources |

Without the operator, a ManagedWorkload's finalizer never runs: its workload stays paused at 0 replicas, and its Services keep routing to the doorman. The guards are hook Jobs that run the operator's image with `--guard`: each waits two and a half minutes for any ManagedWorkloads being deleted, then lists those it would leave without an operator, and fails, which stops Helm before it changes anything. Helm prints the list.

- The `<fullname>-uninstall-guard` Job counts every ManagedWorkload in the watched namespaces, or in every namespace without `watchNamespaces`, and any elsewhere that the operator took on, with its `hybernate.io/cleanup` finalizer. See [Uninstall](../getting-started/installation.md#uninstall). Argo CD 3.3 and later runs it as a PreDelete hook, so deleting the Application is blocked the same way; earlier versions skip it.
- The `<fullname>-upgrade-guard` Job counts those the operator took on in namespaces the new `watchNamespaces` leaves out. See [Narrowing watchNamespaces](../operations/upgrading.md#narrowing-watchnamespaces). Argo CD runs it as a PreSync hook, before every sync.

Each runs as a ServiceAccount of its own name, through a ClusterRole that may only list ManagedWorkloads, which read their metadata cluster-wide, even with `watchNamespaces`, since those they look for may be outside it. The ServiceAccounts and RBAC are hooks too, deleted once the hook has run; a failed Job is kept for an hour, for `kubectl logs`. The hooks use `image`, `imagePullSecrets` and `logLevel`. Meshes' sidecars are kept out of their pods, since a sidecar that keeps running would keep the Job from finishing. Their pods have their own `control-plane: uninstall-guard` and `control-plane: upgrade-guard` labels, so the operator's Service and NetworkPolicy don't select them. `--no-hooks` skips both.

## Pod Scheduling

| Value | Default | Description |
|-------|---------|-------------|
| `nodeSelector` | `{}` | Node selector constraints, for the operator, and for the doorman unless `doorman.nodeSelector` is set |
| `tolerations` | `[]` | Pod tolerations, likewise |
| `affinity` | `{}` | Pod affinity/anti-affinity rules, likewise |
| `podAnnotations` | `{}` | Pod annotations, likewise |
| `podLabels` | `{}` | Pod labels, likewise. They can't replace the labels the chart selects pods by |
| `priorityClassName` | `""` | PriorityClass, likewise. A paused workload's requests wait on the doorman and the operator, so on a cluster that preempts, give them a class above the workloads they wake |
| `extraEnv` | `[]` | Extra environment variables for the operator, as container `env` entries, such as `GOMAXPROCS` |

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
- Read Deployments and StatefulSets, scale them through the scale subresource, and patch them to set and remove the `hybernate.io/paused-from` annotation while they're paused. Hybernate can't delete them, and changes nothing else on them
- Read PersistentVolumeClaims, to price the storage a paused workload keeps
- Read HorizontalPodAutoscalers, and read and annotate KEDA ScaledObjects, to pause a KEDA workload through KEDA
- Read pod metrics from metrics-server
- Read pods, to price what pausing a workload frees, sidecars included. Pods are read when needed rather than watched, so they aren't cached
- Read Nodes' metadata, to price workloads at the list price of the instance types they run on. Only names and labels are cached
- Get ConfigMaps, one at a time and uncached, to [learn dependencies](../concepts/dependencies.md#learned-dependencies) from the ones a workload's environment references. It can't list or change them
- Create Events for user-facing status updates
- Read Services, and manage the EndpointSlices that route paused workloads to the doorman

The doorman has its own ServiceAccount and ClusterRole, `<fullname>-doorman`: it reads ManagedWorkloads and EndpointSlices, gets the pods a woken workload's endpoints name, to check each endpoint's address is its pod's, patches ManagedWorkloads to wake them, and creates Events. With `doorman.admissionPolicy.enabled`, those patches may only set the two annotations that record a request.

With `watchNamespaces` set, both get a Role and RoleBinding in each of those namespaces instead. The operator keeps one ClusterRole, to read Namespaces and Nodes, which only a ClusterRole can grant, and gets a `<fullname>-doorman-endpoints` Role in the release namespace to read the doorman's EndpointSlices. Workloads in other namespaces can't be read, scaled, or opted in, and dependencies on them aren't held or woken. With `metrics.enabled` and `metrics.secure`, both ServiceAccounts also keep their cluster-wide `system:auth-delegator` bindings, which let them check scrapers' tokens and nothing else.

If leader election is enabled, a `<fullname>-leader-election` Role in the release namespace grants Leases (`get`, `create`, `update`) and Events.

For people, the chart adds ManagedWorkloads to Kubernetes' built-in roles, through two aggregated ClusterRoles: `<fullname>-view`, in `view`, `edit` and `admin`, lets whoever can view a namespace `get`, `list` and `watch` its ManagedWorkloads, as `kubectl hybernate status` and `scan` do; `<fullname>-edit`, in `edit` and `admin`, lets whoever can edit it `create`, `update`, `patch` and `delete` them, as `pause`, `wake` and `enable` do. The kustomize manifests' `managedworkload-viewer-role` and `managedworkload-editor-role` are aggregated the same way.

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
