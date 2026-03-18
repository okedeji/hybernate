# Idle Detection

Hybernate uses a multi-layered approach to idle detection. A workload isn't considered idle until multiple independent checks agree — and even then, a grace period must elapse before the operator acts.

## The Four States

```
Active ──► SignalsConfirm ──► InGracePeriod ──► Idle
  ▲              │                  │
  └──────────────┴──────────────────┘
         (any signal denies)
```

| State | Meaning |
|-------|---------|
| **Active** | At least one signal says the workload is busy. No action. |
| **SignalsConfirm** | All signals confirm idle. Waiting for the forecast engine to agree. |
| **InGracePeriod** | Signals and prediction both confirm idle. Counting down the grace period. |
| **Idle** | Grace period has elapsed. The operator executes the configured action (pause or destroy). |

If any signal denies at any point, the state resets to Active and the grace period timer is cleared.

## Signals

Signals are independent checks that answer one question: "Is this workload idle right now?"

### Internal Signal (automatic)

Every ManagedWorkload with an `idlePolicy` gets an automatic CPU threshold check. It reads aggregate pod CPU usage from the Kubernetes Metrics API and compares it against `idlePolicy.idleThreshold` (default: 50 millicores).

- CPU below threshold → confirms idle
- CPU at or above threshold → denies idle

### Prometheus Signals (optional)

You can add application-level checks via PromQL queries. These are defined in `idlePolicy.signals`:

```yaml
idlePolicy:
  signals:
    - source: prometheus
      promQL: 'rate(http_requests_total{service="my-api"}[10m]) == 0'
```

A Prometheus signal confirms idle when the query returns a non-zero result. An empty result or zero value denies idle.

Examples of useful idle signals:

| Signal | PromQL |
|--------|--------|
| No HTTP traffic | `rate(http_requests_total{service="api"}[10m]) == 0` |
| No active WebSocket connections | `sum(websocket_connections{service="api"}) == 0` |
| No messages in queue | `sum(queue_depth{service="worker"}) == 0` |
| No active sessions | `sum(active_sessions{app="dashboard"}) == 0` |

See the [Prometheus Signals Guide](../guides/prometheus-signals.md) for detailed examples.

## Consensus Model

**All signals must confirm for idle detection to proceed.** The first signal that denies stops the evaluation immediately.

This is intentional — false positives (pausing a busy workload) are much more costly than false negatives (leaving an idle workload running a bit longer).

## Prediction Confirmation

After all signals confirm idle, Hybernate checks with the forecast engine. If the engine is in an active phase (DailyActive or beyond), it must also predict low demand for the current hour.

This adds a second layer of confirmation: signals tell you what's happening *right now*, while the prediction engine tells you whether this is *expected*. A workload that's briefly quiet during a normally busy hour won't be paused.

If the engine is still in the Observing phase (not enough data yet), prediction confirmation is skipped and signals alone drive the decision.

## Grace Period

The grace period (`idlePolicy.gracePeriod`) is the final safety gate. Even after signals and prediction both confirm idle, the operator waits for the grace period to elapse before acting.

During the grace period, signals are re-checked on every reconcile. If any signal denies, the timer resets.

```yaml
idlePolicy:
  gracePeriod: "10m"  # Wait 10 minutes of continuous idle before acting
```

A longer grace period is safer but delays cost savings. A shorter one is more responsive but risks false positives. For most workloads, 5-10 minutes is a good starting point.

## Idle Actions

When idle is confirmed, the operator executes the action configured in `idlePolicy.action`:

| Action | Behavior |
|--------|----------|
| `auto` | Same as `pause` — scales the workload to zero |
| `pause` | Scales to zero, captures resource snapshot, sets PausedAt |
| `destroy` | Deletes the workload entirely (with optional PVC retention) |

## Auto-Resume

If `idlePolicy.autoResume` is set to `true`, the operator watches for signals to clear while the workload is paused. When any signal no longer confirms idle (e.g., incoming traffic detected via Prometheus), the workload is automatically resumed.
