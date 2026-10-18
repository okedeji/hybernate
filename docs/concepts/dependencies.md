# Dependencies

Some workloads never see outside traffic: databases, message brokers, caches, workers. Their own [activity clock](idle-detection.md) can run out while the workloads that use them are still busy. Hybernate [learns](#learned-dependencies) who needs what from each workload's environment, and `dependsOn` declares it by hand.

```yaml title="managedworkload.yaml" linenums="1"
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata:
  name: api
  namespace: preview-42
spec:
  target: {kind: Deployment, name: api}
  dependsOn:
    - {kind: StatefulSet, name: postgres}                         # same namespace
    - {namespace: messaging, kind: StatefulSet, name: nats}       # another namespace
  idlePolicy:
    idleAfter: 1h
  prediction: {}                                                  # default confidence
```

With the label instead, set `hybernate.io/depends-on: "statefulset/postgres, messaging/statefulset/nats"` on the workload; see [Opting In](../guides/opt-in.md#settings).

Declare `dependsOn` on the workload that **needs** the other one. Each entry names the dependency's Deployment or StatefulSet, not its ManagedWorkload. `namespace` defaults to the ManagedWorkload's own.

## The rules

**A dependency doesn't pause while anything that depends on it is awake.** When its own clock runs out, it stays up and says why:

```
HeldByDependents=True   kept awake for preview-42/api, preview-43/api
```

Its clock keeps running. Once the last dependent has paused, the dependency pauses on its next check, provided its own clock has run out too.

**Waking a workload wakes its dependencies.** Before the workload starts resuming, Hybernate sets `hybernate.io/last-activity` on each dependency's ManagedWorkload, which wakes it through the usual [activity annotation](idle-detection.md#activity-annotations) path. This chains: if Postgres itself depends on something, that wakes too. You can see it happen with `kubectl describe`.

By default, everything wakes at once, so a chain of dependencies doesn't add up to sequential cold starts.

Waking a dependency never holds up its dependent. One that can't be woken, such as one whose ManagedWorkload a policy engine won't let Hybernate annotate, gets a `DependencyWakeFailed` Warning event on the dependent, which wakes, or is handed back or deleted, all the same.

## Waiting for a dependency to be ready

Some workloads fail if they start before a dependency can serve: an API that gives up after a few connection retries while its database replays its write-ahead log. Set `waitForReady` on that dependency:

```yaml
dependsOn:
  - {kind: StatefulSet, name: postgres, waitForReady: true}
  - {kind: Deployment, name: cache}          # wakes at the same time
```

The dependent then stays in `Resuming`, without scaling up, until every replica of that dependency is Ready, or until it serves with as many as it will have for now:

```
WaitingForDependencies=True   waiting for preview-42/statefulset/postgres (0 ready)
```

A dependency that has some replicas Ready but not all, such as one with a pod crash-looping, serves with those that are, so the dependent doesn't wait for the rest for good. A managed dependency counts once its own [resume](lifecycle.md#automatic) has finished, even short of every replica (its `ResumeIncomplete=True`); one Hybernate doesn't manage counts once the dependent has waited 2 minutes, the time a resume gives the rest of its replicas. A dependency with no replica Ready is always waited for.

The wait is long, because a slow start is exactly why you'd set it, but bounded: a dependency that's up but never Ready, such as one crash-looping or whose image can't be pulled, would otherwise leave the dependent at zero for good. The dependent waits, from when it began resuming, as long as the dependency's Deployment `progressDeadlineSeconds` (600 by default), past which Kubernetes itself reports the rollout failed, or 10 minutes for a StatefulSet, which has none. Then it starts anyway, with `WaitingForDependencies=False`, reason `DependencyWontStart`, and a Warning event naming the dependency:

```
WaitingForDependencies=False   not waiting for preview-42/statefulset/postgres (it isn't Ready after 10m)
```

It doesn't wait at all for a dependency that can't start by waiting: one Hybernate doesn't manage that's scaled to zero, reported the same way. Nor for one that in turn waits, through `waitForReady`, for the dependent, directly or through others: each would wait for the other to start first. That's reason `DependencyCycle`, and both start together. A managed dependency that's still paused is woken again every few seconds while the dependent waits.

## Edge cases

| Situation | What happens |
|-----------|--------------|
| The dependency has no ManagedWorkload | Hybernate never pauses it, so there's nothing to hold or wake |
| The dependency doesn't exist | `DependencyNotFound=True` on the dependent. Nothing is blocked, including a `waitForReady` resume |
| The dependency is in a namespace outside `watchNamespaces`, or Hybernate isn't allowed to read it | `DependencyNotFound=True` with reason `DependencyNotVisible`, naming why. It isn't held or woken, and nothing is blocked |
| A cycle in `dependsOn` (A depends on B, B depends on A) | Each would hold the other awake forever, so neither pauses and both report `DependencyCycle=True` until it's removed |
| A workload listed as its own dependency | Ignored: it's neither a cycle nor something to wait for |
| A namespace's `hybernate.io/depends-on` names several workloads, such as `statefulset/postgres, statefulset/redis` | The workloads it names don't take it: they're the dependencies, not dependents, so postgres and redis don't depend on each other. Neither does a workload one of them depends on by its own annotation: with the namespace naming `deployment/auth` and auth's own annotation naming `statefulset/postgres`, postgres doesn't depend on auth. The namespace's other workloads take the list |
| A cycle through learned dependencies, such as two services that call each other | Not a cycle: a learned dependent doesn't hold a workload it's itself a dependency of. Each pauses when idle, and waking either wakes the other |
| `kubectl hybernate pause` on a dependency that's in use | It isn't paused: the request is answered with `PauseRequest=False`, reason `HeldByDependents`, and a `HeldByDependents` event naming the dependents, as for an idle pause. Pause the dependents first |
| `dryRun: true` | The hold still applies and is reported; nothing is changed |
| A dependent that manages nothing: one being deleted, a duplicate (`DuplicateTarget=True`), or one whose target is gone (`TargetAvailable=False`, reason `TargetNotFound`) | It doesn't hold its dependencies: it has no running workload to keep them up for |
| Many dependents awake at once | `HeldByDependents` names the first 10, by namespace and name, and counts the rest |

## Across namespaces

Dependencies can point into other namespaces, so preview environments can share a database or broker that lives elsewhere. The namespace the dependency is in has to agree, with the `hybernate.io/dependents-from` annotation listing the namespaces whose workloads may depend on its own, or `"*"` for any:

```bash
kubectl annotate namespace shared hybernate.io/dependents-from="preview-42, preview-43"
```

Without it, anyone who can write a ManagedWorkload, or a workload's environment, in one namespace could keep another namespace's workloads awake. A dependency in a namespace that doesn't list the dependent's, declared or learned, isn't held, woken or waited for, and the dependent reports it, without saying whether it exists:

```
DependencyNotAllowed=True   not in a namespace that allows dependents from preview-42 with hybernate.io/dependents-from, so not held or woken: shared/statefulset/postgres
```

A dependency can only be kept awake or woken by its dependents, never paused, scaled down, or deleted, so the most a dependent in another namespace can do is keep it running. `HeldByDependents` always names the holders by namespace.

## Learned dependencies

Hybernate reads the environment of every workload it manages, the way [`kubectl hybernate scan`](../getting-started/kubectl-plugin.md#scan-a-cluster) does: each variable, literal or from a ConfigMap, holding an address that names a Service, in any form cluster DNS gives it (`postgres`, `postgres.preview-42.svc.cluster.local`, or a headless Service's pod, `postgres-0.postgres-hl`), followed to the workload the Service selects. Each one it finds is held awake and woken exactly like a `dependsOn`, with no YAML to write:

```yaml title="status.learnedDependencies"
learnedDependencies:
  from: 3f9c2a71d0e4b815
  at: "2026-10-04T09:12:00Z"
  dependencies:
    - {namespace: preview-42, kind: StatefulSet, name: postgres, source: environment, via: PGHOST, address: postgres-0.postgres-hl}
    - {namespace: preview-42, kind: StatefulSet, name: redis, source: wake}
```

with a `DependenciesLearned` event when the set changes. They're learned again when the pod template changes, and hourly for ConfigMaps changed since. Secrets are never read, so an address set only in a Secret isn't learned from the environment.

**From wakes, too.** When a request wakes a paused workload, the [doorman](wake-on-request.md) records where it came from, and the operator finds the pod that sent it. If that pod belongs to a workload Hybernate manages, that workload depends on the one it woke, and the link is learned with `source: wake`. That covers addresses in Secrets, built in code, or read from files, the first time they're used. A request from a workload Hybernate doesn't manage, such as an ingress controller, or from a pod on the node's network, teaches nothing. Only an address the doorman wrote counts: the operator checks, from the field managers the API server records, that `hybernate.io/last-request-from` was set under Hybernate's own, so one set by hand teaches nothing. The sender is only looked for in the woken workload's namespace and those its [`hybernate.io/dependents-from`](#across-namespaces) allows. Links learned from wakes are kept when the environment is read again, for 14 days after the last request that woke the dependency: `lastSeen` records it, at most once a day. While a link is kept, the dependency is held awake with the workload and woken with it, so the workload's requests no longer reach the doorman and don't renew the link. A link in steady use is dropped too, 14 days after it was learned or last renewed, and learned again from the next request that wakes the dependency, at the cost of that request's cold start, about once a fortnight. A one-off call, such as someone's `curl` from a pod, is forgotten the same way, so it doesn't tie two workloads together for good.

A learned link is safe to apply on its own: a wrong one can only keep a workload awake longer, or wake it, never pause it. To drop one, name it in `hybernate.io/ignore-dependencies` on the workload or its ManagedWorkload, comma-separated, as `namespace/name` or a name in its own namespace:

```yaml
metadata:
  annotations:
    hybernate.io/ignore-dependencies: "redis, messaging/nats"
```

`dependsOn` still applies alongside, and is the only way to set `waitForReady`. [`kubectl hybernate deps`](../getting-started/kubectl-plugin.md#show-a-workloads-dependencies) shows a workload's links in both directions, and where each came from.

## When you need dependsOn

A request through a normal Service wakes a paused workload by itself, through [wake on request](wake-on-request.md), and learned dependencies cover what's in the environment, headless addresses included. `dependsOn` is what you need for the rest:

- **`waitForReady`**, so the first request doesn't wait for the database too, or the app doesn't fail starting before it.
- **Addresses Hybernate hasn't seen used yet**: set in Secrets, built in code, or read from files, before a request to a paused dependency teaches it.
- **A dependency on a workload behind no Service**, which no address names.

[`kubectl hybernate scan`](../getting-started/kubectl-plugin.md#scan-a-cluster) lists the dependencies it finds in each workload's environment, and whether each is declared, connected by Hybernate, or will be once Hybernate manages the workload.
