# Lifecycle

Every ManagedWorkload moves through a defined set of phases. The operator drives transitions based on workload activity, forecasts, and manual overrides. Its one action is to pause: it never deletes a workload or its storage.

## Phases

```
(new) ──► Running ──► Idle ──► Pausing ──► Paused
             ▲                                │
             │                                ▼
             └──────────────────────────── Resuming
```

| Phase | Description |
|-------|-------------|
| _(none)_ | A ManagedWorkload that has just been created has no phase until its first reconcile, which sets `Running`. `kubectl hybernate status` shows it as `creating`. (`Creating` is accepted by the API but never set.) |
| **Running** | Workload is active. Its activity clock is evaluated on each reconcile. |
| **Idle** | No activity for `idleAfter`. The operator pauses it, or in dry-run reports the pause it would make. |
| **Pausing** | What the pause will change is recorded, and the workload is being scaled to zero. |
| **Paused** | Workload is at zero replicas. |
| **Resuming** | Previous replica count is being restored. Transitions to Running when the pods are Ready. |

A workload already at zero replicas, scaled there by a person, a pipeline or KEDA, stays `Running` with the `ScaledToZero` condition: Hybernate only pauses what's running, and only wakes what it paused. See [Workloads Already at Zero](../guides/pause.md#workloads-already-at-zero).

## Transition Triggers

### Automatic (no `desiredState` set)

- **Running → Idle**: No activity for `idleAfter`, no `active-until` hold, no confident forecast of demand, no awake dependents, CPU (and any Prometheus query) readable, and at least one replica; see [Idle Detection](idle-detection.md#the-decision)
- **Idle → Pausing**: Right away, unless the workload is in dry-run, or a GitOps tool undid its last pause within the hour
- **Idle → Running**: Activity resumes (in dry-run, or while a GitOps tool's undoing holds the pause off; otherwise the pause has already started), or the workload is scaled to zero outside Hybernate
- **Paused → Resuming**: A request to its Service, a change to an activity annotation since the pause, `autoResume` ahead of forecast demand, or dry-run being turned on
- **Paused → Running**: Something other than Hybernate scales it up, see [Argo CD and Flux](../guides/gitops.md); or Hybernate lets go of it, see [When Hybernate Lets Go](../guides/pause.md#when-hybernate-lets-go)
- **Resuming → Running**: Every replica it was restored to is Ready

### Manual (`desiredState` set)

- **Running or Idle → Pausing → Paused**: `desiredState: Paused`, unless the workload is already at zero replicas
- **Pausing or Paused → Resuming → Running**, and **Idle → Running**: `desiredState: Running`

Manual overrides take priority over automation, which keeps learning the forecast but doesn't act on it; the `ManualOverride` condition says so. Remove `desiredState` to return to automatic management. See [Pause and Resume](../guides/pause.md).

## Idempotency

Every lifecycle operation is idempotent. What a pause changes is recorded in `status.pause` (`previousReplicas`, the KEDA ScaledObject and its own `paused-replicas` value) in the same status write that enters `Pausing`, before anything is scaled, so that:

- A pause interrupted at any point, by a restart or a conflicting write, is finished or undone from the record
- Resuming restores the recorded replica count, not a hardcoded value
- Re-pausing an already-paused workload is a no-op

This means the operator can crash and restart at any point without leaving workloads in an inconsistent state. `spec.target` can't be changed once set, so a paused target is never left behind at zero: create another ManagedWorkload to manage another workload.

## Status Conditions

The operator sets standard Kubernetes conditions on the CR, each with `Reason`, `Message`, and `LastTransitionTime`. A problem that's resolved is shown as `False` rather than removed.

| Type | `True` means | Reasons |
|------|--------------|---------|
| `TargetAvailable` | The Deployment or StatefulSet exists and Hybernate manages it | `TargetExists`; `False`: `TargetNotFound`, `TargetIgnored` (labelled `hybernate.io/ignore`) |
| `MetricsAvailable` | CPU can be measured, so the activity clock and the forecast can run | `MetricsReported`; `False`: `NoPodMetrics`, `NoCPURequests`, `MetricsUnavailable` |
| `PrometheusAvailable` | The Prometheus activity queries answer; only set when there are some | `QueriesEvaluated`; `False`: `QueryFailed`, `EndpointNotConfigured` |
| `IdleVetoed` | It's idle, but a confident forecast expects demand in the coming hour, so the pause waits; the message names the hour | `ForecastExpectsDemand`; `False`: `NotVetoed` |
| `WakeOnRequest` | Requests to the paused workload's Services wake it | `DoormanRouted`; `False`: see [When a workload isn't routed](wake-on-request.md#when-a-workload-isnt-routed) (`NoServices`, `ServedByOtherPods`, `UnsupportedIPFamily`, `UnsupportedLoadBalancer`, `DoormanUnavailable`, `NoFreePort`, `RoutingFailed`, `DoormanDisabled`), or `NotPaused`, `TargetNotFound` once routing is removed |
| `HeldByDependents` | It's kept awake for the workloads that depend on it | `DependentsAwake`; `False`: `NoAwakeDependents` |
| `WaitingForDependencies` | Its resume waits for a `waitForReady` dependency to be Ready | `DependencyNotReady`; `False`: `DependenciesReady`, or `DependencyWontStart`, naming a dependency it doesn't wait for because it can't start |
| `DependencyCycle` | `dependsOn` forms a cycle through it, so it won't pause | `DependencyCycle`; `False`: `NoCycle` |
| `DependencyNotFound` | A `dependsOn` workload doesn't exist, or can't be seen | `DependencyNotFound`, or `DependencyNotVisible` when it's only outside `watchNamespaces` or not readable; `False`: `DependenciesFound`, `NoDependencies` |
| `DuplicateTarget` | Another, older ManagedWorkload manages the same target, so this one does nothing | `DuplicateTarget`; `False`: `Resolved` |
| `Protected` | Its namespace is protected, so it's never paused | `ProtectedNamespace`; `False`: `NotProtected` |
| `ManualOverride` | `desiredState` is set, so automation doesn't pause or wake it | `DesiredStateSet`; `False`: `Automated` |
| `WouldPause` | In dry-run, `desiredState: Paused` would have paused it | `DryRun`; `False`: `NotHeldBack` |
| `GitOpsConflict` | A GitOps tool undid its last pause; removed once a pause holds. See [Argo CD and Flux](../guides/gitops.md) | `PauseUndone` |
| `Autoscaled` | An HPA or KEDA ScaledObject scales it, and its range; see [HPA and KEDA](../guides/autoscalers.md) | `HPA`, `KEDA` |
| `ScaledToZero` | It was scaled to zero outside Hybernate, so Hybernate leaves it off: it doesn't pause it, wake it, or route requests for it until its replicas are set above zero | `ScaledToZero`; `False`: `HasReplicas` |

## Events

User-visible state changes emit Kubernetes events that show up in `kubectl describe`, each once, when the change happens:

- Lifecycle: `IdleDetected`, `Paused`, `Resumed`, `ActivityResumed` (in dry-run, with what the would-be pause would have freed)
- What woke it: `WokeByActivity`, `WokenByRequest` (from the doorman), `AutoResume`, `ScaledUp`, `DryRunWake`
- Scaled to zero outside Hybernate, and left off: `ScaledToZero`
- What holds it awake: `IdleVetoed`, `HeldByDependents`, `DependencyCycle`, `AutomationSkipped` (when `desiredState` takes over)
- A GitOps tool undoing a pause, and the conflict ending: `GitOpsConflict`, `GitOpsConflictResolved`
- Problems: `TargetNotFound`, `DuplicateTarget`, `NoPodMetrics`, `NoCPURequests`, `MetricsUnavailable`, `QueryFailed`, `EndpointNotConfigured`, `RoutingFailed`, `UnsupportedLoadBalancer`, `RequestNotServed`, `Protected`, `DependentsAwake`
- The forecast: `PredictionFed` each hour it learns, `RegimeChange` when it's demoted, `ForecastReset` when its saved state can't be read
- Learned dependencies: `DependenciesLearned`

Events about the workload's own Deployment or StatefulSet, from the opt-in controller, are on that workload: `Managed`, `NoLongerManaged`, `InvalidSetting`, `LabelIgnored`, `Protected`.

In dry-run, the events for decisions it holds back carry a `[dry-run]` prefix; see [Dry Run](../guides/dry-run.md#events).
