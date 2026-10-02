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
    autoResume: true

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
| `autoResume` | bool | `false` | Wake ahead of the demand a confident forecast predicts |

Deploys and the `hybernate.io/last-activity` and `hybernate.io/active-until` annotations always count as activity; see [Idle Detection](../concepts/idle-detection.md#activity-annotations).

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
