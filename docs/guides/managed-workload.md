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

This is the absolute minimum. The operator will watch the Deployment but won't take any automated action until you add idle or scale policies.

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
    cpuIdleThreshold: 50
    gracePeriod: "10m"
    autoResume: true
    signals:
      - source: prometheus
        promQL: 'rate(http_requests_total{service="my-api"}[10m]) == 0'

  scalePolicy:
    minReplicas: 1
    maxReplicas: 10
    down:
      stabilization: "5m"
      maxStep: 2
      guard:
        - source: prometheus
          promQL: 'sum(active_connections{service="my-api"}) < 10'
    up:
      stabilization: "2m"
      maxStep: 3

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

See [Idle Detection](../concepts/idle-detection.md) for how the detection pipeline works.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `action` | `auto`, `pause`, `destroy` | `auto` | What to do when idle is confirmed |
| `cpuIdleThreshold` | int (millicores) | `50` | CPU usage below this = potentially idle |
| `memoryIdleThreshold` | int64 (bytes) | `104857600` (100Mi) | Memory usage below this = potentially idle |
| `gracePeriod` | duration | _(none)_ | How long signals must continuously confirm before acting |
| `autoResume` | bool | `false` | Auto-resume when signals clear |
| `signals` | list of ProbeSpec | _(none)_ | Additional Prometheus checks |

### `scalePolicy`

See the [Scaling Guide](scaling.md) for detailed behavior.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `minReplicas` | int | `1` | Floor for scaling |
| `maxReplicas` | int | _(required)_ | Ceiling for scaling |
| `overrideReplicas` | int | _(none)_ | Bypass prediction, force this count |
| `down` | ScaleDirectionSpec | _(none)_ | Scale-down constraints |
| `up` | ScaleDirectionSpec | _(none)_ | Scale-up constraints |

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

Controls behavior when replicas are changed externally (e.g., by HPA or a human).

| Value | Behavior |
|-------|----------|
| `enforce` | Correct the drift back to the operator's target |
| `warn` | Emit an event but leave the external change |
| `defer` | Accept the external change and update internal state |

### `dryRun`

When `true`, the operator evaluates all policies and emits events but takes no action. Use this to validate configuration before enabling.

## One Workload Per Target

Only one ManagedWorkload can manage a given Deployment or StatefulSet. If you create a second ManagedWorkload pointing at the same target, the operator will set a `DuplicateTarget` condition and refuse to reconcile.
