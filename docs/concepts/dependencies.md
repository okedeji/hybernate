# Dependencies

Some workloads never see outside traffic: databases, message brokers, caches, workers. Their own [activity clock](idle-detection.md) can run out while the workloads that use them are still busy. Hybernate [learns](#learned-dependencies) who needs what from each workload's environment, and `dependsOn` declares it by hand.

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

**From wakes, too.** When a request wakes a paused workload, the [doorman](wake-on-request.md) records where it came from, and the operator finds the pod that sent it. If that pod belongs to a workload Hybernate manages, that workload depends on the one it woke, and the link is learned with `source: wake`. That covers addresses in Secrets, built in code, or read from files, the first time they're used. A request from a workload Hybernate doesn't manage, such as an ingress controller, or from a pod on the node's network, teaches nothing. Links learned from wakes are kept when the environment is read again.

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
