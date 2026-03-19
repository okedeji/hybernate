# Scaling

Hybernate adjusts replica counts using forecast-driven predictions, not reactive thresholds. Instead of waiting for CPU to spike and then scrambling to add capacity, the forecast engine predicts demand for the next hour and scales proactively. A constraint pipeline then ensures every scaling decision is safe and gradual.

## From Forecast to Replicas

The forecast engine outputs a predicted CPU demand in millicores. This is converted to a replica count:

\[
R = \left\lceil \frac{F(t+1)}{C_{\text{pod}}} \right\rceil
\]

Where \( F(t+1) \) is the forecast for the next hour and \( C_{\text{pod}} \) is the CPU capacity per replica. The ceiling function ensures the system always rounds up, favoring over-provisioning over under-provisioning.

This proposed replica count then passes through a constraint pipeline before any scaling happens.

## The Constraint Pipeline

Every scaling decision passes through four stages in order. A proposal can be blocked, reduced, or clamped at any stage.

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

### 1. Stabilization Window

A cooldown period that prevents oscillation. After a scale event, the operator blocks further scaling in the **same direction** until the window elapses.

The key detail: direction tracking is independent. Scaling up after a recent scale-down is allowed immediately. Only repeated scaling in the same direction is throttled.

| Scenario | Blocked? |
|----------|----------|
| Scaled down 2 min ago, proposing another scale-down (window: 5m) | Yes |
| Scaled down 2 min ago, proposing scale-up (window: 1m) | No |
| Scaled up 30s ago, proposing scale-down (window: 5m) | No |

### 2. Min/Max Clamping

Hard bounds. The operator will never scale below `minReplicas` or above `maxReplicas`, regardless of what the forecast predicts.

### 3. Step Limit

Caps how many replicas can change in a single reconcile. This creates gradual ramps instead of sudden jumps.

If the forecast says you need 20 replicas and you currently have 5, with `maxStep: 5` the progression would be: 5 → 10 → 15 → 20 over multiple reconcile loops.

### 4. Guard Probes (scale-down only)

After the constraint pipeline produces a final target, scale-down decisions must pass one more check: guard probes. These are application-level Prometheus queries that answer questions like "are there active connections?" or "are there in-flight jobs?" before allowing replicas to be removed.

All guards must confirm. If any guard returns a zero or empty result, the scale-down is blocked and re-evaluated on the next reconcile.

Scale-up has no guards because adding capacity is always safe.

A defensive second clamp to `[minReplicas, maxReplicas]` is also applied after step limits, in case the step calculation pushed the target out of bounds. If the final target equals the current count after all constraints, no scaling happens.

## Scale-Up vs Scale-Down Asymmetry

Scaling is intentionally asymmetric. Being slow to remove capacity is safer than being slow to add it.

| Concern | Scale-up | Scale-down |
|---------|----------|------------|
| Guard probes | Not required | All guards must confirm |
| Typical stabilization | Short (1-2m) | Longer (5-10m) |
| Step limit | Usually larger | Usually smaller |

## Override Replicas

Setting `overrideReplicas` bypasses the forecast engine but still passes through the constraint pipeline (stabilization, step limits, min/max clamping). This means an override from 10 to 5 replicas with `maxStep: 2` still ramps down gradually: 10 → 8 → 6 → 5.

One difference: scale-down guard probes are skipped for overrides. Since you're explicitly setting the target, the system trusts your intent.

## External Drift and HPA Coexistence

Hybernate detects when something outside its control changes the replica count (HPA, manual `kubectl scale`, another operator). This is called drift. The `conflictAction` field controls the response:

| Policy | Behavior |
|--------|----------|
| `enforce` | Scale back to what Hybernate decided |
| `warn` | Log a warning event but accept the change |
| `defer` | Accept the change and update internal state |

If you run HPA alongside Hybernate, use `defer`. Hybernate won't fight HPA's decisions and will incorporate the new state into its next evaluation.

## Forecast Phase Requirements

Scaling decisions depend on the forecast engine having enough confidence to make predictions. The engine must reach `DailyActive` phase before scaling actions are applied.

| Engine phase | Scaling behavior |
|-------------|-----------------|
| **Observing** | No scaling. Workload stays at its current replica count |
| **DailySuggesting** | Dry-run only. Proposed scaling is logged but not applied |
| **DailyActive+** | Forecast drives real scaling decisions through the constraint pipeline |

This means a newly created ManagedWorkload won't have its replicas changed until the forecast engine has collected at least 24 hours of data and built sufficient confidence. See [Forecasting](forecasting.md#phase-lifecycle) for details on phase progression.
