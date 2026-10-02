# ManagedWorkload Guide

The ManagedWorkload CR is the core resource in Hybernate. It declares a single Deployment or StatefulSet whose lifecycle Hybernate manages.

## Minimal Example

```yaml title="managedworkload.yaml" linenums="1"
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata:
  name: my-api
  namespace: staging
spec:
  target:
    kind: Deployment
    name: my-api
  prediction:
    confidence: 85
```

This is the absolute minimum. The operator will watch the Deployment but won't take any automated action until you add an idle policy.

## Full Example

```yaml title="managedworkload.yaml" linenums="1"
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata:
  name: my-api
  namespace: staging
spec:
  target:
    kind: Deployment
    name: my-api

  idlePolicy:
    action: pause
    idleAfter: 1h
    activity:
      cpuThreshold: 10
      prometheus:
        - promQL: 'sum(rate(nginx_ingress_controller_requests{exported_service="my-api"}[5m]))'
    autoResume: true

  wake:
    onRequest: true
    maxWait: 2m

  pause:
    expireAfter: "24h"
    expireAction: Resume

  destroy:
    pvcRetention: "168h"
    pvcRetentionWarning: "24h"

  prediction:
    confidence: 85

  costTracking:
    rates:
      cpuPerHour: "0.031"
      memoryPerHour: "0.004"
      storagePerMonth: "0.08"

  conflictAction: warn
  dryRun: false
```

## Spec Reference

### `target`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `kind` | `Deployment` or `StatefulSet` | `Deployment` | The kind of workload to manage |
| `name` | string | _(required)_ | Name of the workload (must be in the same namespace) |

### `desiredState`

Optional manual override. When set, automation stops and the operator drives the workload to this state.

| Value | Effect |
|-------|--------|
| `Running` | Resume the workload (restore previous replicas) |
| `Paused` | Pause the workload (scale to zero) |
| `Destroyed` | Delete the workload |

Remove the field to return to automatic management.

### `idlePolicy`

See [Idle Detection](../concepts/idle-detection.md) for how the activity clock works.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `action` | `pause`, `destroy` | `pause` | What to do once the workload has been idle for `idleAfter` |
| `idleAfter` | duration | `1h` | How long without any activity before acting |
| `activity.cpuThreshold` | int (percent) | `10` | CPU utilization, as % of requests, above which the workload counts as active (0-100) |
| `activity.prometheus[].promQL` | string | _(none)_ | PromQL query; a result above zero counts as activity. See the [Prometheus Activity Guide](prometheus-signals.md) |
| `autoResume` | bool | `false` | Wake ahead of the demand a confident forecast predicts |

Deploys and the `hybernate.io/last-activity` and `hybernate.io/active-until` annotations always count as activity; see [Idle Detection](../concepts/idle-detection.md#activity-annotations).

### `dependsOn`

Workloads this one needs. A dependency isn't paused while this workload is awake, and waking this workload wakes it. See [Dependencies](../concepts/dependencies.md).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `namespace` | string | this namespace | Namespace of the dependency |
| `kind` | `Deployment`, `StatefulSet` | _(required)_ | Kind of the dependency |
| `name` | string | _(required)_ | Name of the dependency's workload |
| `waitForReady` | bool | `false` | Don't scale this workload up on resume until the dependency's pods are Ready |

### `wake`

While the workload is paused, a request to any of its Services wakes it and is answered once it's Ready. See [Wake on Request](../concepts/wake-on-request.md).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `onRequest` | bool | `true` | Hold requests to the paused workload and wake it. When `false`, requests fail while it's paused |
| `maxWait` | duration | `2m` | How long a request is held while the workload wakes. After it, the connection is closed; the wake carries on |

A workload paused with `desiredState: Paused` doesn't wake on request.

### `pause`

See [Pause & Destroy](pause-destroy.md) for detailed behavior.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `expireAfter` | duration | _(none)_ | Max time paused before expiry action |
| `expireAction` | `resume` or `destroy` | `destroy` | What happens when expiry elapses |

### `destroy`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `pvcRetention` | duration | _(none)_ | How long to keep PVCs after destroy |
| `pvcRetentionWarning` | duration | _(none)_ | Emit warning event this long before PVC cleanup |

### `prediction`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `confidence` | int (0-100) | `85` | Minimum accuracy before predictions drive decisions |

### `costTracking`

Cost tracking is always enabled with AWS on-demand defaults. Set `costTracking.rates` to override pricing for your cloud provider.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `rates.cpuPerHour` | quantity | `0.031` | $/vCPU-hour |
| `rates.memoryPerHour` | quantity | `0.004` | $/GiB-hour |
| `rates.storagePerMonth` | quantity | `0.08` | $/GiB-month |

### `conflictAction`

Controls behavior when a paused workload is scaled up outside Hybernate (e.g., by a human or a deploy). While a workload is running, Hybernate doesn't manage its replica count, so changes by the team or an HPA are never treated as drift.

| Value | Behavior |
|-------|----------|
| `enforce` | Scale the workload back to zero |
| `warn` | Emit an event but leave the external change |
| `defer` | Accept the change and treat the workload as running |

### `dryRun`

When `true`, the operator evaluates all policies and emits events but takes no action. Use this to validate configuration before enabling.

## One Workload Per Target

Only one ManagedWorkload can manage a given Deployment or StatefulSet. If you create a second ManagedWorkload pointing at the same target, the operator will set a `DuplicateTarget` condition and refuse to reconcile.
