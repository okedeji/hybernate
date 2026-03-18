# Lifecycle

Every ManagedWorkload moves through a defined set of phases. The operator drives transitions based on signals, predictions, manual overrides, and timers.

## Phases

```
Creating ──► Running ──► Idle ──► Pausing ──► Paused
                │          │                    │
                │          │         ┌──────────┤
                │          │         ▼          ▼
                │          │      Resuming   Destroying ──► Destroyed
                │          │         │
                │          │         ▼
                │          └──── Running
                │
                ▼
             Scaling ──► Running
```

| Phase | Description |
|-------|-------------|
| **Creating** | Initial phase on CR creation. Transitions to Running after first reconcile. |
| **Running** | Workload is active. Idle detection and scaling are evaluated on each reconcile. |
| **Idle** | All signals confirm idle, prediction agrees, and grace period has elapsed. Operator will execute the configured idle action. |
| **Scaling** | Replica count is being adjusted (up or down). Transitions back to Running once the target is ready. |
| **Pausing** | Workload is being scaled to zero. In-progress until replicas reach 0. |
| **Paused** | Workload is at zero replicas. Expiry timers and PVC retention are tracked here. |
| **Resuming** | Previous replica count is being restored. Transitions to Running when pods are ready. |
| **Destroying** | Target workload is being deleted. PVC retention countdown starts here. |
| **Destroyed** | Target workload has been deleted. PVC cleanup happens when retention expires. |

## Transition Triggers

### Automatic (no `desiredState` set)

- **Running → Idle**: Idle detection confirms idle (signals + prediction + grace period)
- **Idle → Pausing**: Idle action is `pause` or `auto`
- **Idle → Destroying**: Idle action is `destroy`
- **Running → Scaling**: Forecast engine recommends a different replica count and signals confirm
- **Paused → Resuming**: Pause expiry elapses with `expireAction: Resume`, or `autoResume` triggers when signals clear
- **Paused → Destroying**: Pause expiry elapses with `expireAction: Destroy`

### Manual (`desiredState` set)

- **Any → Pausing → Paused**: `desiredState: Paused`
- **Any → Resuming → Running**: `desiredState: Running`
- **Any → Destroying → Destroyed**: `desiredState: Destroyed`

Manual overrides take priority over automation. Remove `desiredState` to return to automatic management.

## Idempotency

Every lifecycle operation is idempotent. The operator checkpoints state in the CR status (e.g., `status.pause.previousReplicas`, `status.pause.pausedAt`) so that:

- Re-pausing an already-paused workload is a no-op
- Resuming reads the saved replica count, not a hardcoded value
- Destroy records PVC retention expiry once, not on every reconcile

This means the operator can crash and restart at any point without leaving workloads in an inconsistent state.

## Status Conditions

The operator sets standard Kubernetes conditions on the CR:

| Type | Meaning |
|------|---------|
| `Ready` | The operator is successfully managing this workload |
| `Idle` | The workload has been confirmed idle |
| `Paused` | The workload is currently paused |
| `Degraded` | Something is wrong (target not found, API errors) |

Each condition includes `Reason`, `Message`, and `LastTransitionTime` for debugging.

## Events

User-visible state changes emit Kubernetes events that show up in `kubectl describe`:

- Lifecycle transitions (paused, resumed, destroyed)
- Scaling events (up/down with reason)
- Drift detection (external replica changes)
- PVC retention warnings
- Forecast phase changes
- Anomaly detection
