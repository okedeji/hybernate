# Dependencies

Some workloads never see outside traffic: databases, message brokers, caches, workers. Their own [activity clock](idle-detection.md) can run out while the workloads that use them are still busy. `dependsOn` tells Hybernate who needs what.

```yaml title="managedworkload.yaml" linenums="1"
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
```

Declare `dependsOn` on the workload that **needs** the other one. Each entry names the dependency's Deployment or StatefulSet, not its ManagedWorkload. `namespace` defaults to the ManagedWorkload's own.

## The rules

**A dependency doesn't pause while anything that depends on it is awake.** When its own clock runs out, it stays up and says why:

```
HeldByDependents=True   kept awake for preview-42/api, preview-43/api
```

Its clock keeps running. Once the last dependent has paused, the dependency pauses on its next check, provided its own clock has run out too.

**Waking a workload wakes its dependencies.** Hybernate sets `hybernate.io/last-activity` on each dependency's ManagedWorkload, which wakes it through the usual [activity annotation](idle-detection.md#activity-annotations) path. This chains: if Postgres itself depends on something, that wakes too. You can see it happen with `kubectl describe`.

By default, everything wakes at once, so a chain of dependencies doesn't add up to sequential cold starts.

## Waiting for a dependency to be ready

Some workloads fail if they start before a dependency can serve: an API that gives up after a few connection retries while its database replays its write-ahead log. Set `waitForReady` on that dependency:

```yaml
dependsOn:
  - {kind: StatefulSet, name: postgres, waitForReady: true}
  - {kind: Deployment, name: cache}          # wakes at the same time
```

The dependent then stays in `Resuming`, without scaling up, until every replica of that dependency is Ready:

```
WaitingForDependencies=True   waiting for preview-42/postgres (0 ready)
```

There's no timeout, because a slow start is exactly why you'd set it. If the wait runs past 15 minutes, the `HybernateWorkloadStuck` alert fires.

## Edge cases

| Situation | What happens |
|-----------|--------------|
| The dependency has no ManagedWorkload | Hybernate never pauses it, so there's nothing to hold or wake |
| The dependency doesn't exist | `DependencyNotFound=True` on the dependent. Nothing is blocked, including a `waitForReady` resume |
| A cycle (A depends on B, B depends on A) | Each would hold the other awake forever, so neither pauses and both report `DependencyCycle=True` until it's removed |
| `desiredState: Paused` on a dependency that's in use | Your manual setting wins, with a `DependentsAwake` warning event naming the dependents |
| `dryRun: true` | The hold still applies and is reported; nothing is changed |

## Across namespaces

Dependencies can point into other namespaces, so preview environments can share a database or broker that lives elsewhere. A dependency can only be kept awake or woken by its dependents, never paused, scaled down, or deleted, so the most a dependent in another namespace can do is keep it running. `HeldByDependents` always names the holders by namespace.

## When you need dependsOn

A request through a normal Service wakes a paused workload by itself, through [wake on request](wake-on-request.md): an app's connection to a paused database's Service is held by the doorman and wakes the database, costing that one connection a wait. `dependsOn` is what you need when that can't happen or isn't enough:

- **Headless addresses.** Apps that connect to a pod or a headless Service (`postgres-0.postgres-hl`) go straight to pods, so the doorman never sees the connection. Only `dependsOn` wakes the dependency first.
- **No slow first request.** Waking dependencies with the workload, and `waitForReady`, means the first request doesn't wait for the database too.
- **Holding a dependency awake** while its dependents are, so it doesn't pause under them.

[`kubectl hybernate scan`](../getting-started/kubectl-plugin.md#scan-a-cluster) reads workloads' environment variables and lists the dependencies it finds there, with whether each is declared.
