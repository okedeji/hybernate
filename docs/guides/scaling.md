# Scaling Guide

Hybernate adjusts replica counts based on forecast engine predictions, with multiple safety constraints to prevent oscillation and unsafe scale-downs.

## How Scaling Works

```
Forecast Engine ──► Proposed Target
                         │
                    Signal Consensus
                         │
                    Stabilization Check
                         │
                    Min/Max Clamping
                         │
                    Step Limit
                         │
                    Final Target ──► Scale
```

1. The forecast engine predicts demand for the next hour and proposes a replica count
2. All configured signals must confirm the action (e.g., no active connections before scale-down)
3. Stabilization window is checked — if the workload scaled in the same direction recently, the action is blocked
4. The target is clamped to `[minReplicas, maxReplicas]`
5. Step limits cap how many replicas can change in a single reconcile
6. If the final target differs from the current count, the scale happens

## Configuration

```yaml
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

```yaml
scalePolicy:
  minReplicas: 1   # Never go below 1
  maxReplicas: 50  # Never go above 50
```

### Stabilization Window

Cooldown period after a scale event. Prevents oscillation by blocking same-direction scaling until the window elapses.

```yaml
down:
  stabilization: "5m"   # Wait 5 min after a scale-down before scaling down again
up:
  stabilization: "2m"   # Wait 2 min after a scale-up before scaling up again
```

Scale-down typically needs a longer stabilization than scale-up — being slow to remove capacity is safer than being slow to add it.

### Max Step

Limits how many replicas can be added or removed in a single reconcile. This creates gradual ramp-up/ramp-down instead of sudden jumps.

```yaml
down:
  maxStep: 2   # Remove at most 2 replicas per reconcile
up:
  maxStep: 5   # Add at most 5 replicas per reconcile
```

If the forecast says you need 20 replicas and you currently have 5, with `maxStep: 5` the progression would be: 5 → 10 → 15 → 20 over multiple reconcile loops.

### Guard Probes (scale-down only)

Application-level safety checks that must pass before a scale-down proceeds. Defined as Prometheus queries.

```yaml
down:
  guard:
    - source: prometheus
      promQL: 'sum(active_connections{service="my-api"}) < 10'
    - source: prometheus
      promQL: 'sum(in_flight_jobs{service="my-api"}) == 0'
```

All guards must confirm. If any guard returns a zero or empty result, the scale-down is blocked.

## Override Replicas

Bypass the forecast engine and force a specific replica count:

```yaml
scalePolicy:
  overrideReplicas: 5
  minReplicas: 1
  maxReplicas: 10
```

The override is still subject to min/max clamping, stabilization, and step limits. Remove the field to return to prediction-driven scaling.

## Interaction with Idle Detection

Scaling and idle detection are independent evaluations that run on each reconcile:

- If idle detection confirms idle, the workload is paused (scaled to zero) — this overrides scaling
- If the workload is running and not idle, scaling adjusts replicas based on predictions
- If the workload is paused, scaling is not evaluated

## Interaction with HPA

If you're using Kubernetes HPA alongside Hybernate, set `conflictAction` to control what happens:

- `enforce` — Hybernate overrides HPA decisions (not recommended)
- `warn` — Hybernate logs the conflict but lets HPA's change stand
- `defer` — Hybernate accepts HPA's change and updates its internal state

For most setups, use `defer` if running HPA alongside Hybernate.
