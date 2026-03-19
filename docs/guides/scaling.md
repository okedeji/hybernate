# Scaling Guide

Hybernate adjusts replica counts based on forecast engine predictions, with multiple safety constraints to prevent oscillation and unsafe scale-downs. See [Scaling Concepts](../concepts/scaling.md) for the theory behind the constraint pipeline.

## How Scaling Works

```
Forecast Engine ──► Stabilization Check
                         │
                    Min/Max Clamping
                         │
                    Step Limit
                         │
                    Guard Probes (scale-down only)
                         │
                    Final Target ──► Scale
```

1. The forecast engine predicts demand for the next hour and proposes a replica count
2. Stabilization window is checked. If the workload scaled in the same direction recently, the action is blocked
3. The target is clamped to `[minReplicas, maxReplicas]`
4. Step limits cap how many replicas can change in a single reconcile
5. For scale-down, guard probes must all confirm before proceeding
6. If the final target differs from the current count, the scale happens

The forecast engine must be in `DailyActive` phase or beyond before scaling actions are applied. In `Observing`, no scaling occurs. In `DailySuggesting`, scaling runs in dry-run mode only.

## Configuration

```yaml title="managedworkload.yaml" linenums="1"
spec:
  scalePolicy:
    minReplicas: 2
    maxReplicas: 20
    down:
      stabilization: "5m"
      maxStep: 2
      guard:
        - source: prometheus
          promQL: 'sum(active_connections{service="my-api"}) < 10'
    up:
      stabilization: "1m"
      maxStep: 5
```

## Constraints

### Min/Max Replicas

Hard bounds. The operator will never scale below `minReplicas` or above `maxReplicas`.

```yaml title="managedworkload.yaml" linenums="1"
scalePolicy:
  minReplicas: 1   # Never go below 1
  maxReplicas: 50  # Never go above 50
```

### Stabilization Window

Cooldown period after a scale event. Prevents oscillation by blocking same-direction scaling until the window elapses.

```yaml title="managedworkload.yaml" linenums="1"
down:
  stabilization: "5m"   # Wait 5 min after a scale-down before scaling down again
up:
  stabilization: "2m"   # Wait 2 min after a scale-up before scaling up again
```

Scale-down typically needs a longer stabilization than scale-up because being slow to remove capacity is safer than being slow to add it.

### Max Step

Limits how many replicas can be added or removed in a single reconcile. This creates gradual ramp-up/ramp-down instead of sudden jumps.

```yaml title="managedworkload.yaml" linenums="1"
down:
  maxStep: 2   # Remove at most 2 replicas per reconcile
up:
  maxStep: 5   # Add at most 5 replicas per reconcile
```

If the forecast says you need 20 replicas and you currently have 5, with `maxStep: 5` the progression would be: 5 → 10 → 15 → 20 over multiple reconcile loops.

### Guard Probes (scale-down only)

Application-level safety checks that must pass before a scale-down proceeds. Defined as Prometheus queries.

```yaml title="managedworkload.yaml" linenums="1"
down:
  guard:
    - source: prometheus
      promQL: 'sum(active_connections{service="my-api"}) < 10'
    - source: prometheus
      promQL: 'sum(in_flight_jobs{service="my-api"}) == 0'
```

All guards must confirm. If any guard returns a zero or empty result, the scale-down is blocked. Guard probes are not evaluated for scale-up.

## Override Replicas

Bypass the forecast engine and force a specific replica count:

```yaml title="managedworkload.yaml" linenums="1"
scalePolicy:
  overrideReplicas: 5
  minReplicas: 1
  maxReplicas: 10
```

The override is still subject to min/max clamping, stabilization, and step limits. Scale-down guard probes are skipped for overrides since you're explicitly setting the target. Remove the field to return to prediction-driven scaling.

## Interaction with Idle Detection

Scaling and idle detection are independent evaluations that run on each reconcile:

- If idle detection confirms idle, the workload is paused (scaled to zero), which overrides scaling
- If the workload is running and not idle, scaling adjusts replicas based on predictions
- If the workload is paused, scaling is not evaluated

## Interaction with HPA

If you're using Kubernetes HPA alongside Hybernate, set `conflictAction` to control what happens:

| Policy | Behavior |
|--------|----------|
| `enforce` | Scales back to what Hybernate decided (not recommended with HPA) |
| `warn` | Emits a warning event but accepts the external change |
| `defer` | Accepts the change and updates internal state |

Hybernate doesn't detect HPA specifically. It detects any external replica change as drift. For most setups with HPA, use `defer`.
