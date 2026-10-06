# Pause and Resume

Hybernate's one action is to **pause**: scale a workload to zero replicas. The Deployment or StatefulSet stays, and so do its PVCs and their data; only the pods are removed. Hybernate never deletes a workload or its storage.

## Pause

### What Happens When a Workload Is Paused

1. What the pause will change is recorded in `status.pause`, in the same status write that moves the phase to `Pausing`, before anything is scaled: the replica count (`previousReplicas`), the KEDA ScaledObject and its own `autoscaling.keda.sh/paused-replicas` value if it has one, what each replica requests (for [cost tracking](../concepts/cost-tracking.md)), and the activity annotations as they stand
2. A KEDA ScaledObject, if the workload has one, is held at zero with `autoscaling.keda.sh/paused-replicas: "0"`
3. The workload is scaled to 0 through its scale subresource, under the field manager `hybernate`
4. `status.pause.pausedAt` is set and the phase moves to `Paused`, with a `Paused` event

Because the record is written first, a pause interrupted at any point, by an operator restart or a conflicting write, is finished or undone from the record, and a resume always goes back to the replica count the workload had.

### Triggering a Pause

**Automatically:** once the workload has had no activity for `idlePolicy.idleAfter`; see [Idle Detection](../concepts/idle-detection.md).

**Now:** with [`kubectl hybernate pause`](#pause-now), to wake when it's next used.

Either way it's the same pause, which wakes on a request, on activity, or by `autoResume`. To keep a workload off until someone starts it again, scale it to zero yourself: Hybernate leaves it alone; see [Workloads Already at Zero](#workloads-already-at-zero).

### Pause Now

When you're done with a workload for the day, such as a preview environment, pause it now rather than waiting for `idleAfter`:

```bash
kubectl hybernate pause my-api -n staging
```

The result is an ordinary pause, the same as one the idle clock makes: a request to the workload through the [doorman](../concepts/wake-on-request.md), a change to its [activity annotations](../concepts/idle-detection.md#activity-annotations), `autoResume`, or [`kubectl hybernate wake`](../getting-started/kubectl-plugin.md#wake-a-workload) wakes it. It doesn't need an `idlePolicy`, and it works on a ManagedWorkload created from the `hybernate.io/managed` label as on one you wrote: name the workload, as `kubectl hybernate status` shows it.

The plugin asks by setting the `hybernate.io/pause-requested` annotation on the ManagedWorkload to a token, the time of the request. Any tool can do the same; the value only has to differ from the last request's:

```bash
kubectl annotate managedworkload my-api -n staging --overwrite \
  hybernate.io/pause-requested="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
```

To pause it even when the forecast expects demand within the hour, also set `hybernate.io/pause-overrides-forecast: "true"`; set it, or remove it, with every request, since it applies to the request it comes with.

It's an annotation rather than a spec field so that Argo CD and Flux, which own the spec of a ManagedWorkload kept in Git, leave it alone. Hybernate acts on each value once: it records the value it handled in `status.lastPauseRequest`, which survives an operator restart, and says what came of it in the `PauseRequest` condition.

The request runs the idle clock out now, and the workload is paused by the same rules as an idle pause:

| The workload | What Hybernate does | `PauseRequest` condition |
|--------------|---------------------|--------------------------|
| `Running` or `Idle` | Pauses it, with a `PauseRequested` event, then `Paused` | `True`, `Pausing` |
| A confident forecast expects demand within the hour | Leaves it running and says when the demand is expected, since a pause would likely be woken straight back. `kubectl hybernate pause` asks whether to pause it anyway; a request with `hybernate.io/pause-overrides-forecast: "true"`, which `--yes` sets, pauses it | `False`, `ForecastExpectsDemand` |
| Within the hour Hybernate waits after Argo CD or Flux undid its last pause | Pauses it: you asked. If the tool undoes it again, that's a new [`GitOpsConflict`](gitops.md) | `True`, `Pausing` |
| `Pausing` | Finishes the pause, then marks the request handled | `True`, `AlreadyPaused` |
| `Paused` | Marks the request handled | `True`, `AlreadyPaused` |
| `Resuming` | Lets the wake finish, then pauses it | `True`, `Pausing` |
| In [dry-run](dry-run.md) | Never scales it: counts a would-be pause in `status.dryRun`, moves it to `Idle`, and emits a `[dry-run]` `PauseRequested` event. Activity after the request ends the would-be pause, as it would wake a paused workload | `False`, `DryRun` |
| Held awake by `hybernate.io/active-until` on it or its workload | Leaves it running, with a Warning event | `False`, `ActiveUntil` |
| Depended on by awake workloads | Leaves it running, with a `HeldByDependents` event, as for an idle pause | `False`, `HeldByDependents` |
| In a `dependsOn` cycle | Leaves it running, with a Warning event | `False`, `DependencyCycle` |
| In a [protected namespace](opt-in.md#protected-namespaces) | Leaves it running, with a Warning event | `False`, `Protected` |
| Scaled to zero outside Hybernate | Nothing to do: it's off already | `False`, `ScaledToZero` |
| Its workload is missing, labelled `hybernate.io/ignore`, or managed by another ManagedWorkload | Nothing, with a Warning event | `False`, `TargetNotFound`, `TargetIgnored` or `DuplicateTarget` |

A request Hybernate won't act on is answered once, not retried: ask again once the reason is gone. Activity from before the request doesn't undo it: the pause records the activity annotations as they stood, and only a change made since wakes the workload.

## Resume

Resuming wakes the workload's [dependencies](../concepts/dependencies.md), scales it back to `status.pause.previousReplicas` (kept within its HPA's or ScaledObject's range if that changed), and moves it to `Running` once all those replicas are Ready. A KEDA ScaledObject is held at the restored count until then, and afterwards gets back the `paused-replicas` value it had before the pause, or none.

**Automatically:**

- A request to the workload's Service, through [wake on request](../concepts/wake-on-request.md)
- A change to `hybernate.io/last-activity`, `hybernate.io/last-request` or `hybernate.io/active-until` on the ManagedWorkload or its target since the pause began, whatever time the new value states; [`kubectl hybernate wake`](../getting-started/kubectl-plugin.md#wake-a-workload) sets one
- With `autoResume: true`, 15 minutes ahead of the hour a confident forecast expects demand in
- Something else scaling it up, such as `kubectl scale`: it's counted as activity and the workload goes straight to `Running`; see [Argo CD and Flux](gitops.md) for when that's a GitOps tool
- Turning on [dry-run](dry-run.md), which never leaves a workload paused (`DryRunWake` event)

A paused workload is looked at again at least every 5 minutes, 15 minutes before each hour and on the hour, as well as whenever it or its target changes.

**On demand:**

```bash
kubectl hybernate wake my-api -n staging
```

`wake` marks the workload active, which wakes it and restarts its idle clock, so it pauses again once it's had no activity for `idleAfter`. `--for 2h` keeps it awake for at least that long, as for a demo. To keep a workload running for good, remove its `idlePolicy`, or label it `hybernate.io/ignore: "true"`. See [kubectl Plugin](../getting-started/kubectl-plugin.md#wake-a-workload).

## When Hybernate Lets Go

Hybernate hands a paused workload back, scaled to the replicas it had and released from KEDA without waiting for it to be Ready, and stops routing its Services to the doorman, whenever it stops managing it:

- The ManagedWorkload is deleted, or the `hybernate.io/managed` label that created it is removed
- The workload is labelled `hybernate.io/ignore: "true"`
- Its namespace becomes [protected](opt-in.md#protected-namespaces)

Each of these emits a `Resumed` event saying why and how many replicas it has. No longer managing a workload never leaves it switched off.

If the target itself is deleted while paused, its Services stop routing to the doorman, since nothing would start the pods a held request waits for; `TargetAvailable` turns `False` with reason `TargetNotFound`.

## Workloads Already at Zero

A workload scaled to zero outside Hybernate, by `kubectl scale`, a pipeline, or KEDA with no active trigger, is off on purpose, and Hybernate leaves it that way:

- It isn't paused, whatever its idle clock says, and a [pause request](#pause-now) is answered with `ScaledToZero`: there's nothing running to pause, and a pause would record zero replicas to restore
- It isn't woken: Hybernate only wakes what it paused, so neither a request, an activity annotation, `autoResume` nor `kubectl hybernate wake` scales it up
- Its Services aren't routed to the doorman, so a request to it fails as it would without Hybernate, rather than being held for pods that nothing will start

The ManagedWorkload stays `Running`, with the `ScaledToZero` condition and one `ScaledToZero` event saying so. That makes scaling it to zero yourself the way to keep a workload off until someone starts it. Once its replicas are set above zero, by whoever scaled it down, Hybernate manages it again, and counts that as activity, so its idle clock starts afresh.

Hybernate never scales up a workload it didn't scale down. Handing back a pause recorded at zero replicas, Hybernate releases it at zero, with a `Resumed` event that says so.

## Cost Savings While Paused

Cost tracking is always on. While a workload is paused, what its paused replicas requested in CPU and memory accrues as savings; its PVCs still cost, since they stay. See [Cost Tracking](../concepts/cost-tracking.md).

## Workloads Nobody Comes Back To

A paused workload costs only its storage. To remove one for good, delete it the way it was created: from Git, by the pipeline that made the environment, or with a TTL tool such as kube-janitor. Those know when an environment is finished; Hybernate only knows it's idle.

## Finalizer

The `hybernate.io/cleanup` finalizer is added to every ManagedWorkload, so that deleting one while its workload is paused scales the workload back to the replicas it had first.
