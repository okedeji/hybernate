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

Leaving an annotation out means its default. A value that can't be read, such as `idle-after: "soon"` or `dry-run: "True"`, is reported in an `InvalidSetting` warning event on the workload and skipped, as if it weren't there: the setting comes from the next place in [Which setting wins](#which-setting-wins), and the others still apply. Booleans are exactly `"true"` or `"false"`; durations must be above zero. In `depends-on`, an entry that can't be read is skipped and the rest apply.

`dry-run` is the exception: a value that can't be read turns dry-run on, whatever comes after it, since a typo in the setting meant to stop pauses must never start them.

```yaml
hybernate.io/depends-on: "statefulset/postgres, messaging/statefulset/nats"
```

Settings that need more than one value aren't annotations: Prometheus activity queries, `waitForReady` on a dependency, and cost rates. For those, write a [ManagedWorkload](managed-workload.md) yourself.

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

`enable` sets the workload's own `hybernate.io/dry-run` annotation to `"false"`, which wins over its namespace's annotation and the cluster default, wherever the dry-run came from. `enable --all` sets the namespace's annotation to `"false"` instead, and drops the workloads' own.

If Argo CD or Flux applies the workload or namespace, `enable` doesn't change the cluster, since the tool would put the annotation back. It names the tool, prints the change to make in Git, and exits non-zero; `--force` changes the cluster anyway.

## Protected namespaces

Label a namespace `hybernate.io/protected: "true"`, such as production, and Hybernate never manages anything in it, whatever labels its workloads or the namespace carry. Nothing there is opted in, a ManagedWorkload written for one reports `Protected=True` and is never paused, and a workload Hybernate had paused before the namespace was protected is woken, so protecting a namespace never leaves anything off. A warning event says why on each.

```bash
kubectl label namespace payments hybernate.io/protected=true
```

To protect namespaces by name instead, such as every `prod-*`, set the Helm value `protectedNamespaces`. A protected namespace is managed again only when labelled `hybernate.io/allow-protected: "true"` on purpose; Hybernate never sets either label.

To keep Hybernate out of namespaces altogether, not by its own rule but by Kubernetes RBAC, install it with `watchNamespaces`; see [Helm values](../reference/helm-values.md#namespaces).

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

- **Labelled:** creates a ManagedWorkload named after the workload, owned by it, marked with `hybernate.io/from-label: "true"`, and announces it with a `Managed` event on the workload. If that name is taken, for example by a StatefulSet's of the same name as a Deployment, it's named after the workload and its kind instead, such as `api-statefulset`.
- **Annotations changed:** updates the ManagedWorkload within seconds. The annotations are the source of truth for what they set: `dryRun`, `idlePolicy` (`idleAfter`, `activity.cpuThreshold`, `autoResume`), which workloads `dependsOn` lists, and `wake`. Direct edits to those are overwritten. Everything else is yours to set on the ManagedWorkload and is kept: `desiredState`, `prediction`, `costTracking`, `idlePolicy.activity.prometheus`, and `waitForReady` on a dependency the annotation lists. The target never changes.
- **Label removed, or `hybernate.io/ignore` added:** deletes the ManagedWorkload. If the workload is paused, it's first scaled back to the replicas it had.
- **Workload deleted:** its ManagedWorkload goes with it.
- **A ManagedWorkload you wrote for the workload exists:** yours wins, and the label does nothing.
- **A label value other than `"true"`**, such as `hybernate.io/managed: "dry-run"`, doesn't opt the workload in, and is pointed out in an event. Settings like dry-run are annotations.
