# Argo CD and Flux

Hybernate pauses a workload by setting its replicas to zero. If Argo CD or Flux applies that workload from Git with `replicas` in its manifest, it sees the cluster no longer matches Git and sets them back, undoing the pause. Each tool has a one-time setting that leaves the replicas to Hybernate; this page gives it.

Teams that already run an HPA or KEDA under Argo CD or Flux usually have it in place, since an autoscaler changes replicas too.

## What Hybernate does

Hybernate never fights Git. When a paused workload is scaled up by anything other than Hybernate, it counts as activity: the workload goes back to Running, and `status.lastScaledUp` records when, how many replicas, and what did it, by the field manager the API server recorded (`kubectl-scale`, `argocd-controller`, `kustomize-controller`, ...).

When that was Argo CD or Flux, the workload also reports it, with the fix:

```
GitOpsConflict=True   Argo CD set the replicas from Git, undoing the pause; Hybernate pauses it again from 14:20 UTC. ...
```

along with a `GitOpsConflict` warning event. Hybernate waits an hour before pausing again, so it doesn't restart the workload in a loop with the tool. Pausing again is also how it finds out the fix is in: once a pause holds for an hour, or until Hybernate wakes it, the condition clears with a `GitOpsConflictResolved` event.

Before you opt anything in, [`kubectl hybernate scan`](../getting-started/kubectl-plugin.md#scan-a-cluster) lists the workloads whose replicas are set from Git, by tool.

Hybernate's writes are recorded under the field manager `hybernate`, which is what the Argo CD setting below refers to.

## Argo CD

Have Argo CD ignore the replicas Hybernate sets, once for every app, in the `argocd-cm` ConfigMap:

```yaml title="argocd-cm" linenums="1"
data:
  resource.customizations.ignoreDifferences.all: |
    managedFieldsManagers:
      - hybernate
```

Then add `RespectIgnoreDifferences=true` to each app's sync options, so a sync leaves them too and not only the diff:

```yaml title="application.yaml" linenums="1"
spec:
  syncPolicy:
    syncOptions:
      - RespectIgnoreDifferences=true
```

Ignoring by field manager leaves Argo CD in charge of every other field, and of replicas changed by anything but Hybernate. If your apps already ignore `/spec/replicas`, for example for an HPA, that covers Hybernate as well:

```yaml
spec:
  ignoreDifferences:
    - group: apps
      kind: Deployment
      jsonPointers: [/spec/replicas]
```

## Flux

Flux only sets the fields a manifest has. Leave `replicas` out of the workload's manifest in Git, and Flux leaves them to Hybernate; Kubernetes starts a new Deployment at one replica.

For a Helm chart that always renders `replicas`, remove it in the HelmRelease with a post-renderer:

```yaml title="helmrelease.yaml" linenums="1"
spec:
  postRenderers:
    - kustomize:
        patches:
          - target: {kind: Deployment, name: my-api}
            patch: |
              - op: remove
                path: /spec/replicas
```
