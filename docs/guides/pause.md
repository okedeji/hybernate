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

**Manually:**

```bash
kubectl patch managedworkload my-api -n staging \
  --type merge -p '{"spec":{"desiredState":"Paused"}}'
```

This works on a ManagedWorkload created from the `hybernate.io/managed` label too: the opt-in controller keeps `desiredState`, which no annotation sets. Find a label-created ManagedWorkload's name with `kubectl get managedworkloads -n staging`; it's the workload's name, or `<name>-<kind>` such as `api-statefulset` when that's taken.

A workload paused with `desiredState` stays paused while it's set: activity, requests and `autoResume` don't wake it, and something else scaling it up is paused again. Set it to `Running` to wake it. Removing it instead hands the workload back to automation still paused, to wake like any paused workload, on a request or activity. Its `ManualOverride` condition says so. If workloads that depend on it are awake, the pause goes ahead anyway, with a `DependentsAwake` warning event.

In [dry-run](dry-run.md), `desiredState: Paused` doesn't scale anything: the `WouldPause` condition and one `[dry-run]` event say what would have happened.

## Resume

Resuming wakes the workload's [dependencies](../concepts/dependencies.md), scales it back to `status.pause.previousReplicas` (kept within its HPA's or ScaledObject's range if that changed), and moves it to `Running` once all those replicas are Ready. A KEDA ScaledObject is held at the restored count until then, and afterwards gets back the `paused-replicas` value it had before the pause, or none.

**Automatically:**

- A request to the workload's Service, through [wake on request](../concepts/wake-on-request.md)
- A change to `hybernate.io/last-activity`, `hybernate.io/last-request` or `hybernate.io/active-until` on the ManagedWorkload or its target since the pause began, whatever time the new value states, or an `active-until` still in the future; [`kubectl hybernate wake`](../getting-started/kubectl-plugin.md#wake-a-workload) sets one
- With `autoResume: true`, 15 minutes ahead of the hour a confident forecast expects demand in
- Something else scaling it up, such as `kubectl scale`: it's counted as activity and the workload goes straight to `Running`; see [Argo CD and Flux](gitops.md) for when that's a GitOps tool
- Turning on [dry-run](dry-run.md), which never leaves a workload paused (`DryRunWake` event)

A paused workload is looked at again at least every 5 minutes, 15 minutes before each hour and on the hour, as well as whenever it or its target changes.

**Manually:**

```bash
kubectl patch managedworkload my-api -n staging \
  --type merge -p '{"spec":{"desiredState":"Running"}}'
```

The workload then stays running: automation doesn't pause it until `desiredState` is removed:

```bash
kubectl patch managedworkload my-api -n staging \
  --type json -p '[{"op":"remove","path":"/spec/desiredState"}]'
```

To wake a workload once and leave automation in charge, use `kubectl hybernate wake my-api -n staging` instead.

## When Hybernate Lets Go

Hybernate hands a paused workload back, scaled to the replicas it had and released from KEDA without waiting for it to be Ready, and stops routing its Services to the doorman, whenever it stops managing it:

- The ManagedWorkload is deleted, or the `hybernate.io/managed` label that created it is removed
- The workload is labelled `hybernate.io/ignore: "true"`
- Its namespace becomes [protected](opt-in.md#protected-namespaces)

Each of these emits a `Resumed` event saying why and how many replicas it has. No longer managing a workload never leaves it switched off.

If the target itself is deleted while paused, its Services stop routing to the doorman, since nothing would start the pods a held request waits for; `TargetAvailable` turns `False` with reason `TargetNotFound`.

## Workloads Already at Zero

A workload scaled to zero outside Hybernate, by `kubectl scale`, a pipeline, or KEDA with no active trigger, is off on purpose, and Hybernate leaves it that way:

- It isn't paused, whatever its idle clock or `desiredState: Paused` says: there's nothing running to pause, and a pause would record zero replicas to restore
- It isn't woken: Hybernate only wakes what it paused, so neither a request, an activity annotation, `autoResume` nor `desiredState: Running` scales it up
- Its Services aren't routed to the doorman, so a request to it fails as it would without Hybernate, rather than being held for pods that nothing will start

The ManagedWorkload stays `Running`, with the `ScaledToZero` condition and one `ScaledToZero` event saying so. Once its replicas are set above zero, by whoever scaled it down, Hybernate manages it again, and counts that as activity, so its idle clock starts afresh.

Hybernate never scales up a workload it didn't scale down. Handing back a pause recorded at zero replicas, Hybernate releases it at zero, with a `Resumed` event that says so.

## Cost Savings While Paused

Cost tracking is always on. While a workload is paused, what its paused replicas requested in CPU and memory accrues as savings; its PVCs still cost, since they stay. See [Cost Tracking](../concepts/cost-tracking.md).

## Workloads Nobody Comes Back To

A paused workload costs only its storage. To remove one for good, delete it the way it was created: from Git, by the pipeline that made the environment, or with a TTL tool such as kube-janitor. Those know when an environment is finished; Hybernate only knows it's idle.

## Finalizer

The `hybernate.io/cleanup` finalizer is added to every ManagedWorkload, so that deleting one while its workload is paused scales the workload back to the replicas it had first.
