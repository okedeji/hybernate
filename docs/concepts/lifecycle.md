# Lifecycle

Every ManagedWorkload moves through a defined set of phases. The operator drives transitions based on workload activity, forecasts, and pauses asked for with `kubectl hybernate pause`. Its one action is to pause: it never deletes a workload or its storage.

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
| **Idle** | No activity for `idleAfter`, or a pause was requested. The operator pauses it, or in dry-run reports the pause it would make. |
| **Pausing** | What the pause will change is recorded, and the workload is being scaled to zero and its pods stopped. |
| **Paused** | Workload is at zero replicas. |
| **Resuming** | Previous replica count is being restored. Transitions to Running when the pods are Ready, or when Hybernate stops waiting for those that aren't. |

A workload already at zero replicas, scaled there by a person, a pipeline or KEDA, stays `Running` with the `ScaledToZero` condition: Hybernate only pauses what's running, and only wakes what it paused. See [Workloads Already at Zero](../guides/pause.md#workloads-already-at-zero).

## Transition Triggers

### Automatic

- **Running → Idle**: No activity for `idleAfter`, no `active-until` hold, no confident forecast of demand, no awake dependents, CPU (and any Prometheus query) readable, and at least one replica; see [Idle Detection](idle-detection.md#the-decision)
- **Idle → Pausing**: Right away, unless the workload is in dry-run, or a GitOps tool undid its last pause within the hour
- **Idle → Running**: Activity resumes (in dry-run, or while a GitOps tool's undoing holds the pause off; otherwise the pause has already started), or the workload is scaled to zero outside Hybernate
- **Paused → Resuming**: A request to its Service, a change to an activity annotation since the pause, `autoResume` ahead of forecast demand, or dry-run being turned on
- **Paused → Running**: Something other than Hybernate scales it up, see [GitOps tools](../guides/gitops.md); or Hybernate lets go of it, see [When Hybernate Lets Go](../guides/pause.md#when-hybernate-lets-go). The same goes for a workload still `Pausing` once Hybernate has scaled it to zero (`status.pause.pausedAt` is set), while its pods stop: it isn't scaled back down
- **Resuming → Running**: Something other than Hybernate scales it back to zero after the resume has scaled it up (`status.pause.resumedAt`): it's left off, with `ScaledToZero`, rather than scaled up again. Otherwise, every replica it was restored to is Ready. Short of that, Hybernate stops waiting 2 minutes after the first replica is Ready, or, while none is, once the Deployment's `progressDeadlineSeconds` has passed since the scale-up (10 minutes by default, and for a StatefulSet). Stopping short sets `ResumeIncomplete` until every replica is Ready, and with none Ready, requests fail until one is, since the doorman no longer holds them. A StatefulSet with the default `OrderedReady` pod management starts its pods one at a time, so a slow-starting one may have only its first pods Ready when the 2 minutes are up: it runs with those, and the rest follow. The wait starts once its `waitForReady` dependencies serve; see [Dependencies](dependencies.md)

### On request

- **Running or Idle → Pausing → Paused**: `kubectl hybernate pause`, which sets a new `hybernate.io/pause-requested` value. The idle clock runs out at once, and the workload is paused by the rules of an idle pause, except that the hour after a GitOps conflict doesn't hold it back. A forecast expecting demand within the hour declines it, saying when, unless the request overrides the forecast. In dry-run it goes **Running → Idle** instead, as a would-be pause. See [Pause Now](../guides/pause.md#pause-now)
- **Paused → Resuming → Running**: `kubectl hybernate wake`, which changes an activity annotation, or any of the automatic wakes above: a requested pause wakes like any other

## Idempotency

Every lifecycle operation is idempotent. What a pause changes is recorded in `status.pause` (`previousReplicas`, the KEDA ScaledObject and its own `paused-replicas` value) in the same status write that enters `Pausing`, before anything is scaled, so that:

- A pause interrupted at any point, by a restart or a conflicting write, is finished or undone from the record
- Resuming restores the recorded replica count, not a hardcoded value
- Re-pausing an already-paused workload is a no-op
- Before the workload is scaled, the record is also copied to its `hybernate.io/paused-from` annotation, until it's handed back. A ManagedWorkload that loses its status while paused, to a restore without status or by being created again, takes the pause up from that copy (`PauseRecovered`): back to `Paused` if the workload is still at zero, or released if it has been scaled up since. One left `Pausing`, `Paused` or `Resuming` with neither had nothing scaled, and is `Running` again (`PauseRecordLost`)

This means the operator can crash and restart at any point without leaving workloads in an inconsistent state. `spec.target` can't be changed once set, so a paused target is never left behind at zero: create another ManagedWorkload to manage another workload.

## Status Conditions

The operator sets standard Kubernetes conditions on the CR, each with `Reason`, `Message`, and `LastTransitionTime`. A condition appears once it first applies. After that, one that no longer applies is shown as `False`, with the reason listed after `False` below, rather than removed; only `GitOpsConflict` and `Autoscaled` are removed instead.

| Type | `True` means | Reasons |
|------|--------------|---------|
| `TargetAvailable` | The Deployment or StatefulSet exists and Hybernate manages it | `TargetExists`; `False`: `TargetNotFound`, `TargetIgnored` (labelled `hybernate.io/ignore`) |
| `MetricsAvailable` | CPU can be measured, so the activity clock and the forecast can run | `MetricsReported`; `False`: `NoPodMetrics`, `NoCPURequests`, `MetricsUnavailable` |
| `PrometheusAvailable` | The Prometheus activity queries answer; only set when there are some | `QueriesEvaluated`; `False`: `QueryFailed`, `EndpointNotConfigured` |
| `PrometheusQueryEmpty` | A Prometheus activity query has matched no series since it was first evaluated, which usually means a misspelt metric or label; informational, it doesn't stop a pause | `NoSeries`; `False`: `QueriesMatchSeries` |
| `IdleVetoed` | It's idle, but a confident forecast expects demand in the coming hour, so the pause waits; the message names the hour | `ForecastExpectsDemand`; `False`: `NotVetoed` |
| `WakeOnRequest` | Requests to the paused workload's Services wake it | `DoormanRouted`; `False`: see [When a workload isn't routed](wake-on-request.md#when-a-workload-isnt-routed) (`NoServices`, `ServedByOtherPods`, `UnsupportedIPFamily`, `UnsupportedLoadBalancer`, `DoormanUnavailable`, `NoFreePort`, `TooManyPorts`, `RoutingFailed`, `DoormanDisabled`, `NodeLocalTraffic`), or `NotPaused`, `TargetNotFound`, `DuplicateTarget` once routing is removed |
| `HeldByDependents` | It's kept awake for the workloads that depend on it | `DependentsAwake`; `False`: `NoAwakeDependents` |
| `WaitingForDependencies` | Its resume waits for a `waitForReady` dependency to be Ready | `DependencyNotReady`; `False`: `DependenciesReady`; `DependencyWontStart`, naming a dependency it doesn't wait for because it can't start or wasn't Ready in time; or `DependencyCycle`, naming one that waits for it in turn |
| `DependencyCycle` | `dependsOn` forms a cycle through it, so it won't pause | `DependencyCycle`; `False`: `NoCycle` |
| `DependencyNotFound` | A `dependsOn` workload doesn't exist, or can't be seen | `DependencyNotFound`, or `DependencyNotVisible` when it's only outside `watchNamespaces` or not readable; `False`: `DependenciesFound`, `NoDependencies` |
| `DependencyNotAllowed` | A dependency, declared or learned, is in another namespace whose `hybernate.io/dependents-from` doesn't list this one, so it isn't held, woken or waited for | `DependencyNotAllowed`; `False`: `DependenciesAllowed`, `NoDependencies` |
| `DuplicateTarget` | Another, older ManagedWorkload manages the same target, so this one does nothing, after handing back a pause it had made | `DuplicateTarget`; `False`: `Resolved` |
| `Protected` | Its namespace is protected, it's Hybernate's own operator or doorman, or it's a StatefulSet that deletes its PVCs when scaled down, so it's never paused | `ProtectedNamespace`, `HybernateComponent`, `PVCsDeletedOnScaleDown`; `False`: `NotProtected` |
| `PauseRequest` | The last [pause request](../guides/pause.md#pause-now) paused the workload, or found it paused; `False` says why it didn't | `Pausing`, `AlreadyPaused`; `False`: `DryRun`, `ForecastExpectsDemand`, `ActiveUntil`, `HeldByDependents`, `DependencyCycle`, `Protected`, `ScaledToZero`, `TargetNotFound`, `TargetIgnored`, `DuplicateTarget` |
| `GitOpsConflict` | A GitOps tool undid its last pause; removed once a pause holds for an hour. See [GitOps tools](../guides/gitops.md) | `PauseUndone` |
| `ResumeIncomplete` | The last resume stopped waiting with some replicas not Ready, and the workload runs with those that are, until they all are; see [Workload resumed with replicas not Ready](../operations/troubleshooting.md#workload-resumed-with-replicas-not-ready) | `ReplicasNotReady`; `False`: `AllReplicasReady` |
| `PauseIncomplete` | The last pause stopped waiting for its pods with some still running, after their termination grace period and a minute more, and the workload is `Paused` regardless; see [Workload paused with pods still running](../operations/troubleshooting.md#workload-paused-with-pods-still-running) | `PodsStillRunning`; `False`: `PodsStopped`, `NotPaused` |
| `Autoscaled` | An HPA or KEDA ScaledObject scales it, and its range; removed when neither does. See [HPA and KEDA](../guides/autoscalers.md) | `HPA`, `KEDA` |
| `ScaledToZero` | It was scaled to zero outside Hybernate, so Hybernate leaves it off: it doesn't pause it, wake it, or route requests for it until its replicas are set above zero | `ScaledToZero`; `False`: `HasReplicas` |

## Events

User-visible state changes emit Kubernetes events that show up in `kubectl describe`, each once, when the change happens:

- Lifecycle: `IdleDetected`, `PauseRequested`, `Paused`, `Resumed`, `ActivityResumed` (in dry-run, with what the would-be pause would have freed)
- A pause record lost from status: `PauseRecovered` (taken up again from the target's `hybernate.io/paused-from`), `PauseRecordLost` (a warning: nothing to finish it from)
- A pause request Hybernate didn't act on: an event whose reason says why, the same as the `PauseRequest` condition's
- What woke it: `WokeByActivity`, `WokenByRequest` (from the doorman), `AutoResume`, `ScaledUp`, `DryRunWake`
- Scaled to zero outside Hybernate, and left off: `ScaledToZero`
- What holds it awake: `IdleVetoed`, `HeldByDependents`, `DependencyCycle`
- A GitOps tool undoing a pause, and the conflict ending: `GitOpsConflict`, `GitOpsConflictResolved`
- Dependencies: `DependencyWontStart` and `DependencyCycle` (warnings: a resume that starts without waiting for a `waitForReady` dependency), `DependencyWakeFailed` (a warning: a dependency couldn't be woken)
- Problems: `TargetNotFound`, `DuplicateTarget`, `NoPodMetrics`, `NoCPURequests`, `MetricsUnavailable`, `QueryFailed`, `EndpointNotConfigured`, `PrometheusQueryEmpty`, `InvalidActivityAnnotation`, `RoutingFailed`, `UnsupportedLoadBalancer`, `RequestNotServed`, `Protected`, `ResumeIncomplete` (a resume that stopped waiting for replicas not Ready, in place of `Resumed`; `AllReplicasReady` once they are), `PauseIncomplete` (a pause that stopped waiting for pods that didn't stop, in place of `Paused`), `HandBackAbandoned` (a ManagedWorkload in a namespace being deleted letting go of its workload)
- The forecast: `PredictionFed` each hour it learns, `RegimeChange` when it's demoted, `ForecastReset` when its saved state can't be read
- Learned dependencies: `DependenciesLearned`

Events about the workload's own Deployment or StatefulSet, from the opt-in controller, are on that workload: `Managed`, `NoLongerManaged`, `InvalidSetting`, `LabelIgnored`, `Protected`.

In dry-run, the events for decisions it holds back carry a `[dry-run]` prefix; see [Dry Run](../guides/dry-run.md#events).
