# Opting In

Install Hybernate once per cluster, then label what it should manage. The label says **which** workloads Hybernate manages; annotations say **how**.

```yaml title="deployment.yaml"
apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout-api
  namespace: preview-42
  labels:
    hybernate.io/managed: "true"
  annotations:
    hybernate.io/dry-run: "true"      # measure first; nothing is paused
    hybernate.io/idle-after: "2h"
```

The label is usually one line in your Helm values or manifests, in a file you already keep in Git. Hybernate then creates a [ManagedWorkload](managed-workload.md) for the workload and keeps it in step with the annotations.

## A whole namespace

Label the namespace to opt in every Deployment and StatefulSet in it. Annotations on the namespace set the defaults for its workloads:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: preview-42
  labels:
    hybernate.io/managed: "true"
  annotations:
    hybernate.io/dry-run: "true"
    hybernate.io/idle-after: "2h"
```

This covers dev and preview namespaces, and third-party charts that don't let you add labels to their workloads. To leave one workload out of a managed namespace, label it `hybernate.io/ignore: "true"`.

## Settings

| Annotation | Values | Default | Description |
|------------|--------|---------|-------------|
| `hybernate.io/dry-run` | `"true"`, `"false"` | `"false"` | Measure without pausing: the activity clock and savings run as normal, but the workload is never paused |
| `hybernate.io/idle-after` | duration, e.g. `"90m"`, `"2h"` | `1h` | How long without activity before pausing |
| `hybernate.io/cpu-threshold` | whole number, `"1"` to `"100"` | `10` | CPU use, as a percentage of requests, above which the workload counts as active |
| `hybernate.io/auto-resume` | `"true"`, `"false"` | `"false"` | Wake ahead of the demand a confident [forecast](../concepts/forecasting.md) predicts |
| `hybernate.io/depends-on` | comma-separated `kind/name` or `namespace/kind/name` | none | Workloads this one needs; see [Dependencies](../concepts/dependencies.md) |
| `hybernate.io/wake-on-request` | `"true"`, `"false"` | `"true"` | Wake when a request reaches the workload's Service; see [Wake on Request](../concepts/wake-on-request.md) |
| `hybernate.io/wake-max-wait` | duration | `2m` | How long a request is held while the workload wakes |
| `hybernate.io/wake-page` | `"true"`, `"false"` | `"true"` | Show browsers a waking-up page |

Leaving an annotation out means its default. A value that can't be read, such as `idle-after: "soon"`, is reported in a warning event on the workload, and that setting falls back to its default; the others still apply.

```yaml
hybernate.io/depends-on: "statefulset/postgres, messaging/statefulset/nats"
```

Settings that delete things or need more than one value aren't annotations: destroying idle workloads, Prometheus activity queries, `waitForReady` on a dependency, pause expiry, cost rates, and `conflictAction`. For those, write a [ManagedWorkload](managed-workload.md) yourself.

### Which setting wins

1. The workload's own annotation.
2. Its namespace's annotation.
3. The cluster-wide defaults, set in the Helm values (`defaults.idleAfter`, `defaults.cpuThreshold`, `defaults.dryRun`).
4. The built-in default.

So a namespace can measure everything with `dry-run: "true"` while one workload in it goes live with `dry-run: "false"`.

## From measuring to pausing

Start with `hybernate.io/dry-run: "true"`. Hybernate runs each workload's activity clock without pausing anything, and keeps a summary of the pauses it would have made in its ManagedWorkload's `status.dryRun`: how many, how long it would have slept, and what that would have freed. `kubectl hybernate scan` lists the same for every workload in dry-run. See [Dry Run](dry-run.md#what-dry-run-measured). When you're ready:

```bash
kubectl hybernate enable checkout-api -n preview-42
kubectl hybernate enable --all -n preview-42     # every workload in the namespace
```

`enable` removes the dry-run annotation, or, when the dry-run comes from the namespace, sets the workload's own to `"false"`. Removing the annotation and setting `"false"` mean the same.

If Argo CD or Flux applies the workload, `enable` doesn't change the cluster, since the tool would put the annotation back. It names the tool and prints the change to make in Git instead.

## With GitOps

The decision to manage a workload lives in Git, as the label. The ManagedWorkload Hybernate creates from it is owned by the workload, like the ReplicaSets a Deployment creates, so Argo CD and Flux leave it alone and Argo CD shows it under the workload. Hybernate never copies the workload's labels onto it, so a GitOps tool's tracking label can't make the tool claim and prune it.

Argo CD and Flux do notice the replica count of a paused workload changing, and would scale it back up. Tell them to leave `spec.replicas` alone:

- **Argo CD:** ignore the field in the Application, and keep the ignored field out of syncs:

    ```yaml
    spec:
      ignoreDifferences:
        - group: apps
          kind: Deployment
          jsonPointers:
            - /spec/replicas
        - group: apps
          kind: StatefulSet
          jsonPointers:
            - /spec/replicas
      syncPolicy:
        syncOptions:
          - RespectIgnoreDifferences=true
    ```

- **Flux:** leave `replicas` out of the workload's manifest. Flux applies with server-side apply, so a field the manifest doesn't set stays with whoever set it last: Hybernate, while the workload is paused. Many charts let you leave it out, for example when their autoscaling value is on.

## What Hybernate does with the label

- **Labelled:** creates a ManagedWorkload named after the workload, owned by it, marked with `hybernate.io/from-label: "true"`, and announces it with a `Managed` event on the workload.
- **Annotations changed:** updates the ManagedWorkload within seconds. Edits made directly to that ManagedWorkload are overwritten: the annotations are the source of truth.
- **Label removed, or `hybernate.io/ignore` added:** deletes the ManagedWorkload. If the workload is paused, it's first scaled back to the replicas it had.
- **Workload deleted:** its ManagedWorkload goes with it.
- **A ManagedWorkload you wrote for the workload exists:** yours wins, and the label does nothing.
- **A label value other than `"true"`**, such as `hybernate.io/managed: "dry-run"`, doesn't opt the workload in, and is pointed out in an event. Settings like dry-run are annotations.
