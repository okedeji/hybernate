# Idle Detection

Hybernate uses a multi-layered approach to idle detection. Real-time signals, forecast-based demand prediction, and a grace period must all agree before the operator acts. This consensus model is designed to prevent false positives.

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
| **InGracePeriod** | Signals and forecast both confirm idle. Counting down the grace period. |
| **Idle** | Grace period has elapsed. The operator executes the configured action (pause or destroy). |

If any signal denies at any point, the state resets to Active and the grace period timer is cleared.

## Signals

Signals are independent checks that answer one question: "Is this workload idle right now?"

### Internal Signal (automatic)

Every ManagedWorkload with an `idlePolicy` gets automatic CPU and memory threshold checks. They read aggregate pod resource usage from the Kubernetes Metrics API and compare against `idlePolicy.cpuIdleThreshold` (default: 10% of CPU request) and `idlePolicy.memoryIdleThreshold` (default: 10% of memory request). Both must be below their respective percentage-of-request thresholds for idle detection to confirm.

- CPU below threshold → confirms idle
- CPU at or above threshold → denies idle

### Prometheus Signals (optional)

You can add application-level checks via PromQL queries. These are defined in `idlePolicy.signals`:

```yaml title="managedworkload.yaml" linenums="1"
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

**All signals must confirm for idle detection to proceed.** The first signal that denies stops the evaluation immediately and resets the state to Active.

After signals reach consensus, the forecast engine must also agree before the grace period starts. Unlike a signal denial (which resets state), a forecast denial is a soft block: the state stays at `SignalsConfirm` and is re-evaluated.

This layered approach is intentional. False positives (pausing a busy workload) are much more costly than false negatives (leaving an idle workload running a bit longer).

## Forecast Confirmation

After all signals confirm idle, Hybernate checks with the [forecast engine](forecasting.md). The forecast is not a signal, but a separate confirmation gate that must pass before the grace period begins.

If the engine is in an active phase (`DailyActive` or beyond), it must predict demand below the idle threshold for the current hour. If the forecast predicts high demand, even though signals say the workload is quiet right now, the grace period will not start. The state stays at `SignalsConfirm` and is rechecked after 5 minutes.

This is the key difference between signals and the forecast: signals tell you what's happening *right now*, while the forecast tells you whether this quiet period is *expected*. A workload that's briefly quiet during a normally busy hour won't be paused because the forecast catches the fluke.

**Phase behavior:**

| Engine phase | Forecast gate |
|-------------|---------------|
| **Observing** | Entire automation is paused, no idle checks run |
| **DailySuggesting** | Idle checks run in dry-run mode. Forecast returns 0, so the gate is effectively open |
| **DailyActive+** | Forecast returns real predictions. The gate blocks if predicted demand exceeds the idle threshold |

As the forecast engine matures through its [phase lifecycle](forecasting.md#phase-lifecycle), it progressively tightens the idle detection criteria. Early on, signals drive decisions alone. Once the engine builds confidence, it acts as an informed second opinion that prevents premature action.

## Grace Period

The grace period (`idlePolicy.gracePeriod`) is the final safety gate. Even after signals and forecast both confirm idle, the operator waits for the grace period to elapse before acting.

During the grace period, signals are re-checked on every reconcile. If any signal denies, the timer resets.

```yaml title="managedworkload.yaml" linenums="1"
idlePolicy:
  gracePeriod: "10m"  # Wait 10 minutes of continuous idle before acting
```

A longer grace period is safer but delays cost savings. A shorter one is more responsive but risks false positives. For most workloads, 5-10 minutes is a good starting point.

## Idle Actions

When idle is confirmed, the operator executes the action configured in `idlePolicy.action`:

| Action | Behavior |
|--------|----------|
| `auto` | Same as `pause` (scales the workload to zero) |
| `pause` | Scales to zero, captures resource snapshot, sets PausedAt |
| `destroy` | Deletes the workload entirely (with optional PVC retention) |

## Auto-Resume

If `idlePolicy.autoResume` is set to `true`, the forecast engine drives the resume decision. Since there are no running pods during a pause, pod-level signals don't exist. Instead, the operator checks the forecast prediction for the current hour. If predicted demand exceeds the idle threshold, the workload is resumed proactively, before traffic arrives.

This means a workload paused overnight can be automatically resumed at 9am if the forecast has learned that Monday mornings are busy. The forecast engine retains its learned patterns across pauses because its state is persisted in the CR status.

Auto-resume follows the same phase rules as idle detection:

| Engine phase | Resume behavior |
|-------------|----------------|
| **Observing** | No resume. Not enough data to make a prediction |
| **DailySuggesting** | Dry-run only. Logs the resume decision but does not act |
| **DailyActive+** | Resumes the workload when predicted demand exceeds the idle threshold |
