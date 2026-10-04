# HPA and KEDA

Hybernate works alongside a HorizontalPodAutoscaler or a KEDA ScaledObject, with nothing to set up. While a workload runs, its autoscaler decides how many replicas it needs, and Hybernate leaves the replica count alone. Once it's idle, Hybernate pauses it to zero, and on a wake brings it back within the autoscaler's range.

The ManagedWorkload says what scales it:

```
Autoscaled=True   HPA web scales it between 2 and 10 replicas; Hybernate pauses it at zero, where the HPA stops scaling, and resumes it within that range
```

## HPA

An HPA stops scaling a workload at zero replicas, so a pause holds: Hybernate scales the workload to zero, and the HPA leaves it there. On a wake, Hybernate restores the replicas it had, kept within the HPA's `minReplicas` and `maxReplicas` should those have changed while it was paused, and the HPA takes over again.

CPU from the workload's own pods counts as activity whatever the HPA does, so a workload busy enough for its HPA to scale it up isn't idle.

## KEDA

KEDA scales its workloads itself, and would scale a paused one straight back up to its `minReplicaCount`, or as soon as a trigger fired. So for a workload a ScaledObject scales, Hybernate pauses through KEDA: it sets `autoscaling.keda.sh/paused-replicas: "0"` on the ScaledObject, which has KEDA hold the workload at zero and stop scaling it, and records the ScaledObject in `status.pause.scaledObject`.

On a wake, Hybernate scales the workload back up first, then removes the annotation, so KEDA carries on from there. A ScaledObject with `minReplicaCount: 0` would otherwise leave a woken workload at zero until a trigger fired, and a request the doorman held would wait for nothing.

The annotation isn't in your manifests, so Argo CD and Flux leave it alone.

### KEDA or Hybernate?

KEDA already scales a workload to zero when its triggers say there's no work, such as an empty queue. Hybernate adds what KEDA doesn't do: waking on the first HTTP request, holding dependencies awake, the forecast, and pausing by activity it can see without a trigger. A workload KEDA already takes to zero gains little; a KEDA workload with a `minReplicaCount` above zero, such as one that should stay warm during working hours, is where pausing it saves.

## Permissions

The operator reads HPAs, and reads and annotates ScaledObjects. Without KEDA installed there are no ScaledObjects, and Hybernate looks for them again every 10 minutes in case it's installed later.
