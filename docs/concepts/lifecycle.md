# Lifecycle

Every ManagedWorkload moves through a defined set of phases. The operator drives transitions based on workload activity, forecasts, and manual overrides. Its one action is to pause: it never deletes a workload or its storage.

## Phases

```
Creating ──► Running ──► Idle ──► Pausing ──► Paused
                ▲                               │
                │                               ▼
                └────────────────────────── Resuming
```

| Phase | Description |
|-------|-------------|
| **Creating** | Initial phase on CR creation. Transitions to Running after first reconcile. |
| **Running** | Workload is active. Its activity clock is evaluated on each reconcile. |
| **Idle** | No activity for `idleAfter`. The operator pauses it, or in dry-run reports the pause it would make. |
| **Pausing** | Workload is being scaled to zero. |
| **Paused** | Workload is at zero replicas. |
| **Resuming** | Previous replica count is being restored. Transitions to Running when the pods are Ready. |

## Transition Triggers

### Automatic (no `desiredState` set)

- **Running → Idle**: No activity for `idleAfter`, and no confident forecast of demand
- **Idle → Pausing**: Right away, unless the workload is in dry-run, is held awake by its dependents, or a GitOps tool undid its last pause within the hour
- **Idle → Running**: Activity resumes (dry-run only; otherwise the pause has already started)
- **Paused → Resuming**: A request to its Service, an activity annotation newer than the pause, or `autoResume` ahead of forecast demand
- **Paused → Running**: Something other than Hybernate scales it up; see [Argo CD and Flux](../guides/gitops.md)

### Manual (`desiredState` set)

- **Any → Pausing → Paused**: `desiredState: Paused`
- **Any → Resuming → Running**: `desiredState: Running`

Manual overrides take priority over automation. Remove `desiredState` to return to automatic management.

## Idempotency

Every lifecycle operation is idempotent. The operator checkpoints state in the CR status (e.g., `status.pause.previousReplicas`, `status.pause.pausedAt`) so that:

- Re-pausing an already-paused workload is a no-op
- Resuming reads the saved replica count, not a hardcoded value

This means the operator can crash and restart at any point without leaving workloads in an inconsistent state.

## Status Conditions

The operator sets standard Kubernetes conditions on the CR, each with `Reason`, `Message`, and `LastTransitionTime`:

| Type | Meaning |
|------|---------|
| `TargetAvailable` | The Deployment or StatefulSet exists |
| `MetricsAvailable` | CPU can be measured, so the activity clock can run |
| `PrometheusAvailable` | The Prometheus activity queries answer, when configured |
| `IdleVetoed` | It's idle, but a confident forecast expects demand within the hour, so the pause waits; the message says when |
| `WakeOnRequest` | Requests to the paused workload's Services wake it |
| `HeldByDependents` | It's kept awake for the workloads that depend on it |
| `WaitingForDependencies` | Its resume waits for a dependency to be Ready |
| `DependencyCycle`, `DependencyNotFound` | A problem with `dependsOn` |
| `DuplicateTarget` | Another ManagedWorkload manages the same target |
| `GitOpsConflict` | A GitOps tool undid its last pause; see [Argo CD and Flux](../guides/gitops.md) |
| `Autoscaled` | An HPA or KEDA ScaledObject scales it, and its range; see [HPA and KEDA](../guides/autoscalers.md) |

## Events

User-visible state changes emit Kubernetes events that show up in `kubectl describe`:

- Lifecycle transitions (idle, paused, resumed)
- Wakes, and what woke it
- A scale-up outside Hybernate, and a GitOps tool undoing a pause
- Forecast phase changes
- Anomaly detection
