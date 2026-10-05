# Labels & Annotations Reference

## Labels

| Label | Value | Applied To | Description |
|-------|-------|-----------|-------------|
| `hybernate.io/managed` | `"true"` | Deployments, StatefulSets, Namespaces | Opts the workload, or every Deployment and StatefulSet in the namespace, in. Hybernate creates a ManagedWorkload for it from its annotations. Any other value doesn't opt in, and is pointed out in an event. See [Opting In](../guides/opt-in.md). |
| `hybernate.io/ignore` | `"true"` | Deployments, StatefulSets | Leaves the workload out, even in a managed namespace. A ManagedWorkload you wrote for it reports `TargetAvailable=False`, reason `TargetIgnored`, and does nothing; a workload Hybernate had paused is scaled back up first. |
| `hybernate.io/protected` | `"true"` | Namespaces | Hybernate never manages anything in the namespace, whatever its workloads' labels, and wakes what it had paused there. See [Protected namespaces](../guides/opt-in.md#protected-namespaces). |
| `hybernate.io/allow-protected` | `"true"` | Namespaces | Lets Hybernate manage a namespace that's labelled protected or matches `protectedNamespaces`. Set on purpose; Hybernate never sets it. |
| `hybernate.io/from-label` | `"true"` | ManagedWorkloads | Set on ManagedWorkloads Hybernate created from the `managed` label. Hybernate updates and deletes only these; ManagedWorkloads without it are yours. |
| `hybernate.io/managed-workload` | ManagedWorkload name | EndpointSlices | Set on the EndpointSlices that route a paused workload's Services to the doorman, along with `endpointslice.kubernetes.io/managed-by: doorman.hybernate.io`. A name longer than 63 characters is shortened, with a hash. They're removed when the workload is Running. |

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

Set on an opted-in Deployment or StatefulSet, or on its Namespace to apply to every workload in it. Each setting is the first value that can be read from: the workload's annotation, its namespace's, the [Helm defaults](helm-values.md#opt-in-defaults) (only `idle-after`, `cpu-threshold` and `dry-run` have one), and the built-in default. See [Opting In](../guides/opt-in.md#settings).

| Annotation | Accepted values | Default | Description |
|------------|-----------------|---------|-------------|
| `hybernate.io/dry-run` | exactly `"true"` or `"false"` | `"false"` | Measure without pausing |
| `hybernate.io/idle-after` | a Go duration above zero, such as `"90m"` or `"2h"` | `1h` | How long without activity before pausing |
| `hybernate.io/cpu-threshold` | a whole number, `"1"` to `"100"` | `10` | CPU use, as a percentage of requests, above which the workload counts as active |
| `hybernate.io/auto-resume` | exactly `"true"` or `"false"` | `"false"` | Wake ahead of forecast demand |
| `hybernate.io/depends-on` | comma-separated `kind/name` or `namespace/kind/name`, kind `deployment` or `statefulset` | none | Workloads this one needs. An empty value declares none, so a workload can drop its namespace's |
| `hybernate.io/wake-on-request` | exactly `"true"` or `"false"` | `"true"` | Wake when a request reaches the workload's Service |
| `hybernate.io/wake-max-wait` | a Go duration above zero | `2m` | How long a request is held while the workload wakes |
| `hybernate.io/wake-page` | exactly `"true"` or `"false"` | `"true"` | Show browsers a waking-up page |

A value that can't be read, such as `"True"`, `"yes"`, `"soon"` or `"0"`, is reported in an `InvalidSetting` warning event on the workload and skipped: the setting comes from the next place in the order above, never from the default in place of a namespace's value, and the other settings still apply. Two exceptions:

- **`dry-run`**: a value that can't be read turns dry-run on, wherever it is, since a typo in the setting meant to stop pauses must never start them.
- **`depends-on`**: an entry that can't be read is skipped and the rest apply; only a value with no readable entry at all falls through to the namespace's.

## Activity Annotations

| Annotation | Value | Applied To | Description |
|------------|-------|-----------|-------------|
| `hybernate.io/last-activity` | RFC 3339 time | ManagedWorkloads, Deployments, StatefulSets | Activity seen by another tool, such as a developer portal. A time in the future counts as now. A change while the workload is paused wakes it. See [Activity annotations](../concepts/idle-detection.md#activity-annotations) |
| `hybernate.io/active-until` | RFC 3339 time | ManagedWorkloads, Deployments, StatefulSets | Keeps the workload awake until that time, and wakes it if paused |
| `hybernate.io/last-request` | RFC 3339 time | ManagedWorkloads | Set by the doorman when it holds a request for a paused workload. Wakes it like `last-activity`, and the clock records the wake as `request`. See [Wake on Request](../concepts/wake-on-request.md) |
| `hybernate.io/last-request-from` | IP address | ManagedWorkloads | Set by the doorman with `last-request`: where the request that woke the workload came from, so the operator can [learn](../concepts/dependencies.md#learned-dependencies) which workload depends on it |

A value that isn't an RFC 3339 time is ignored and logged.

## Other Annotations

| Annotation | Value | Applied To | Description |
|------------|-------|-----------|-------------|
| `hybernate.io/ignore-dependencies` | comma-separated `namespace/name`, or a name in the workload's own namespace | Deployments, StatefulSets, ManagedWorkloads | Dependencies Hybernate shouldn't learn for this workload. See [Learned dependencies](../concepts/dependencies.md#learned-dependencies) |
| `hybernate.io/doorman-ignore-ports` | comma-separated port names or numbers | Services | Ports the doorman never routes, such as a metrics port something else scrapes. They fail while the workload is paused. See [Wake on Request](../concepts/wake-on-request.md#what-wakes-a-workload-and-what-doesnt) |

## Finalizers

| Finalizer | Applied To | Description |
|-----------|-----------|-------------|
| `hybernate.io/cleanup` | ManagedWorkloads | Added automatically by the operator. Ensures a paused workload is scaled back up, and its Services stop routing to the doorman, before the ManagedWorkload can be deleted. The operator removes it once that's done. |

!!! note
    Do not remove the `hybernate.io/cleanup` finalizer by hand. A ManagedWorkload deleted without it leaves a paused workload at zero replicas, and its KEDA ScaledObject held there.
