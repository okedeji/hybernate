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
    confidence: 75
```

This is the absolute minimum: `target` and `prediction` are required, and `prediction: {}` takes the default confidence. The operator will watch the Deployment, track its cost and learn its forecast, but won't pause it on its own until you add an idle policy; [`kubectl hybernate pause`](pause.md#pause-now) pauses it when you ask.

For most workloads you don't need to write one: the `hybernate.io/managed` label creates one from annotations; see [Opting In](opt-in.md). Write one yourself for settings annotations don't cover, such as Prometheus queries, `waitForReady`, or cost rates. A ManagedWorkload you write for a workload wins over the label.

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
    idleAfter: 1h
    activity:
      cpuThreshold: 10
      prometheus:
        - promQL: 'sum(rate(nginx_ingress_controller_requests{exported_service="my-api"}[5m]))'
    autoResume: true

  wake:
    onRequest: true
    maxWait: 2m
    page: true

  prediction:
    confidence: 75

  costTracking:
    rates:
      cpuPerHour: "0.031"
      memoryPerHour: "0.004"
      storagePerMonth: "0.08"

  dryRun: false
```

## Spec Reference

### `target`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `kind` | `Deployment` or `StatefulSet` | `Deployment` | The kind of workload to manage |
| `name` | string | _(required)_ | Name of the workload (must be in the same namespace) |

`target` can't be changed once the ManagedWorkload exists, since a paused target would be left at zero; the API server refuses the edit. To manage another workload, create another ManagedWorkload.

### `idlePolicy`

See [Idle Detection](../concepts/idle-detection.md) for how the activity clock works.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `idleAfter` | duration | `1h` | How long without any activity before pausing |
| `activity.cpuThreshold` | int (percent) | `10` | CPU utilization, as % of requests, above which the workload counts as active (1-100) |
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
| `page` | bool | `true` | Answer a browser loading a page with a waking-up page that reloads until the workload is Running. Other requests are held either way |

### `prediction`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `confidence` | int (50-100) | `75` | Accuracy, as 1 − WAPE, a season's forecasts must reach before they drive decisions. See [Forecasting](../concepts/forecasting.md) |

`prediction` itself is required; `prediction: {}` takes the default.

### `costTracking`

Cost tracking is always on, priced at the list price of the nodes the workload runs on, or AWS on-demand defaults where there's none. Set `costTracking.rates` to override pricing with what you pay; each rate you set wins over the others. See [Cost Tracking](../concepts/cost-tracking.md#prices).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `rates.cpuPerHour` | quantity | `0.031` | $/vCPU-hour |
| `rates.memoryPerHour` | quantity | `0.004` | $/GiB-hour |
| `rates.storagePerMonth` | quantity | `0.08` | $/GiB-month |

### Scaled up outside Hybernate

A paused workload scaled up by anything else, such as `kubectl scale` or a deploy, wakes: whoever did it wants it running. `status.lastScaledUp` records when and what did it. When that was Argo CD or Flux setting replicas from Git, see [Argo CD and Flux](gitops.md). While a workload is running, Hybernate doesn't manage its replica count, so changes by its team or an HPA are left alone.

### `dryRun`

When `true`, the operator evaluates all policies and emits events but never pauses the workload, and wakes it if it had. Use this to validate configuration before enabling. See [Dry Run](dry-run.md).

## One Workload Per Target

Only one ManagedWorkload can manage a given Deployment or StatefulSet. If you create a second ManagedWorkload pointing at the same target, the newer one gets a `DuplicateTarget` condition naming the other, and does nothing until the older one is gone.
