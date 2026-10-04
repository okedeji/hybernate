# Labels & Annotations Reference

## Labels

| Label | Value | Applied To | Description |
|-------|-------|-----------|-------------|
| `hybernate.io/managed` | `"true"` | Deployments, StatefulSets, Namespaces | Opts the workload, or every Deployment and StatefulSet in the namespace, in. Hybernate creates a ManagedWorkload for it from its annotations. Any other value doesn't opt in, and is pointed out in an event. See [Opting In](../guides/opt-in.md). |
| `hybernate.io/ignore` | `"true"` | Deployments, StatefulSets | Leaves the workload out, even in a managed namespace. |
| `hybernate.io/from-label` | `"true"` | ManagedWorkloads | Set on ManagedWorkloads Hybernate created from the `managed` label. Hybernate updates and deletes only these; ManagedWorkloads without it are yours. |
| `hybernate.io/managed-workload` | ManagedWorkload name | EndpointSlices | Set on the EndpointSlices that route a paused workload's Services to the doorman, along with `endpointslice.kubernetes.io/managed-by: doorman.hybernate.io`. They're removed when the workload is Running. |

### Usage

Opt a workload in, or a whole namespace:

```bash
kubectl label deployment my-api hybernate.io/managed=true
kubectl label namespace preview-42 hybernate.io/managed=true
```

Leave one workload in a managed namespace out:

```bash
kubectl label deployment my-critical-service hybernate.io/ignore=true
```

Find the ManagedWorkloads created from labels:

```bash
kubectl get managedworkloads -l hybernate.io/from-label=true --all-namespaces
```

## Settings Annotations

Set on an opted-in Deployment or StatefulSet, or on its Namespace to apply to every workload in it. The workload's own wins over the namespace's, which wins over the [Helm defaults](helm-values.md#opt-in-defaults).

| Annotation | Value | Default | Description |
|------------|-------|---------|-------------|
| `hybernate.io/dry-run` | `"true"`, `"false"` | `"false"` | Measure without pausing |
| `hybernate.io/idle-after` | duration | `1h` | How long without activity before pausing |
| `hybernate.io/cpu-threshold` | `"1"` to `"100"` | `10` | CPU use, as a percentage of requests, above which the workload counts as active |
| `hybernate.io/auto-resume` | `"true"`, `"false"` | `"false"` | Wake ahead of forecast demand |
| `hybernate.io/depends-on` | comma-separated `kind/name` or `namespace/kind/name` | none | Workloads this one needs |
| `hybernate.io/wake-on-request` | `"true"`, `"false"` | `"true"` | Wake when a request reaches the workload's Service |
| `hybernate.io/wake-max-wait` | duration | `2m` | How long a request is held while the workload wakes |
| `hybernate.io/wake-page` | `"true"`, `"false"` | `"true"` | Show browsers a waking-up page |

A value that can't be read is reported in an `InvalidSetting` warning event on the workload, and that setting falls back to its default.

## Activity Annotations

| Annotation | Value | Applied To | Description |
|------------|-------|-----------|-------------|
| `hybernate.io/last-activity` | RFC 3339 time | ManagedWorkloads, Deployments, StatefulSets | Activity seen by another tool, such as a developer portal. A time after the pause wakes a paused workload. See [Activity annotations](../concepts/idle-detection.md#activity-annotations). |
| `hybernate.io/ignore-dependencies` | comma-separated names | Deployments, StatefulSets, ManagedWorkloads | Dependencies Hybernate shouldn't learn for this workload, comma-separated, as `namespace/name` or a name in its own namespace. See [Learned dependencies](../concepts/dependencies.md#learned-dependencies). |
| `hybernate.io/active-until` | RFC 3339 time | ManagedWorkloads, Deployments, StatefulSets | Keeps the workload awake until that time, and wakes it if paused. |
| `hybernate.io/last-request` | RFC 3339 time | ManagedWorkloads | Set by the doorman when it holds a request for a paused workload. Wakes it like `last-activity`, and the clock records the wake as `request`. See [Wake on Request](../concepts/wake-on-request.md). |

## Finalizers

| Finalizer | Applied To | Description |
|-----------|-----------|-------------|
| `hybernate.io/cleanup` | ManagedWorkloads | Added automatically by the operator. Ensures a paused workload is scaled back up before the ManagedWorkload CR can be deleted. The operator removes it after cleanup is done. |

!!! note
    Do not manually remove the `hybernate.io/cleanup` finalizer. If you do, PVCs scheduled for retention cleanup may be orphaned.
