# Installation

## Prerequisites

- Kubernetes v1.30+ (tested in CI on 1.30 and 1.37)
- kubectl v1.30+
- [metrics-server](https://github.com/kubernetes-sigs/metrics-server) installed (Hybernate reads pod CPU via the Kubernetes Metrics API)
- Helm v3.8+ (if using Helm install), for OCI charts

## Install

=== "Helm"

    Install directly from the OCI registry; there's no `helm repo` to add:

    ```bash
    helm install hybernate oci://ghcr.io/okedeji/charts/hybernate \
      --version 0.2.0 \
      --namespace hybernate-system \
      --create-namespace
    ```

    The chart's version has no `v`; the image it runs is tagged with one, such as `v0.2.0`. The chart installs the ManagedWorkload CRD too, and upgrades it with each `helm upgrade`.

    ??? tip "Helm Values"

        | Value | Default | Description |
        |-------|---------|-------------|
        | `replicaCount` | `1` | Number of operator replicas |
        | `image.repository` | `ghcr.io/okedeji/hybernate` | Container image |
        | `image.tag` | the chart's `appVersion` | Image tag |
        | `leaderElection.enabled` | `true` | Enable HA leader election |
        | `metrics.secure` | `true` | Serve metrics over HTTPS |
        | `metrics.readerSubjects` | `[]` | Who may scrape the metrics, such as your Prometheus's ServiceAccount |
        | `timezone` | `UTC` | Time zone the forecast learns hours and weekdays in |
        | `watchNamespaces` | `[]` | Namespaces to work in; empty means all |
        | `defaults.idleAfter` | `1h` | Idle time before pausing, for opted-in workloads that don't set their own |
        | `defaults.cpuThreshold` | `10` | CPU percentage of requests that counts as active |
        | `defaults.dryRun` | `false` | Measure every opted-in workload without pausing, unless it sets its own |
        | `resources.limits.cpu` | `500m` | CPU limit |
        | `resources.limits.memory` | `512Mi` | Memory limit |

        Every value is in the [Helm Values Reference](../reference/helm-values.md).

=== "kubectl"

    Apply the all-in-one installer manifest directly:

    ```bash
    kubectl apply -f https://github.com/okedeji/hybernate/releases/latest/download/install.yaml
    ```

    This installs the CRD, RBAC, the operator Deployment, and the doorman Deployment, Service and PodDisruptionBudget into the `hybernate-system` namespace. The operator is `deployment/hybernate-controller-manager`.

=== "Source"

    Clone the repo and build the image:

    ```bash
    git clone https://github.com/okedeji/hybernate.git
    cd hybernate
    make docker-build IMG=hybernate:dev
    ```

    The image is only in your local Docker, so make it available to the cluster's nodes before deploying. For a [Kind](https://kind.sigs.k8s.io/) cluster, load it into the nodes:

    ```bash
    kind load docker-image hybernate:dev --name <cluster>
    make deploy IMG=hybernate:dev
    ```

    For any other cluster, push it to a registry the nodes can pull from:

    ```bash
    make docker-build docker-push IMG=<registry>/hybernate:dev
    make deploy IMG=<registry>/hybernate:dev
    ```

    `make deploy` installs the CRD, RBAC, the operator and the doorman, all running `IMG`. It writes `IMG` into `config/manager/kustomization.yaml`; `git checkout config/manager` puts it back.

    `kubectl apply -k config/network-policy` adds the NetworkPolicies the chart's `networkPolicy.enabled` creates: only namespaces labelled `metrics: enabled` may scrape the operator and the doorman, and the doorman's routing ports (TCP 20000-29999) stay open to every caller of a paused workload's Services.

## Verify Installation

```bash
# Check the operator and the doorman are running
kubectl get pods -n hybernate-system

# Verify the CRD is installed
kubectl get crd managedworkloads.hybernate.io
```

## Uninstall

Stop managing workloads while the operator is still running, so it can scale any paused ones back up:

1. Remove the `hybernate.io/managed` label from your workloads and namespaces, in Git if that's where they live. Hybernate deletes the ManagedWorkloads it created from them.
2. Delete the ManagedWorkloads you wrote yourself:

    ```bash
    kubectl delete managedworkloads --all --all-namespaces
    ```

    Deleting a ManagedWorkload scales its workload back to the replicas it had before it was paused.

3. Remove the operator:

    ```bash
    helm uninstall hybernate -n hybernate-system   # or: make undeploy
    ```

    `helm uninstall` keeps the CRD, since deleting it deletes every ManagedWorkload. Once none is left, delete it yourself:

    ```bash
    kubectl delete crd managedworkloads.hybernate.io
    ```

    `make undeploy`, and `kubectl delete -f install.yaml`, delete the CRD along with the operator.

`helm uninstall` refuses to go ahead while any ManagedWorkload is left. A pre-delete hook, the `<fullname>-uninstall-guard` Job, first waits up to two and a half minutes for ManagedWorkloads being deleted, so the operator can restore them and drain their doorman ports, which takes up to a minute; then, if any are left, it lists them and fails, so the uninstall stops with `job hybernate-uninstall-guard failed` before removing anything. A ManagedWorkload deleted just before you uninstall doesn't hold it up. Above that error, Helm prints the ManagedWorkloads left, as they are after the wait, and the steps above; older Helm releases, such as 3.16, don't print a hook's output, and `kubectl logs job/hybernate-uninstall-guard -n hybernate-system` shows it, for an hour. With `watchNamespaces`, it counts those in the watched namespaces, and any elsewhere that the operator took on, with its `hybernate.io/cleanup` finalizer. The hook runs the operator's own image. Pass `--no-hooks` to skip it, or set `guard.uninstall: false` to drop it.

A refused uninstall leaves the release marked `uninstalling`, though everything still runs. Uninstalling again, once the ManagedWorkloads are gone, finishes it. Until then `helm upgrade` fails with `has no deployed releases`; `helm rollback hybernate <revision>`, with the last revision `helm history hybernate` lists, marks it deployed again. Argo CD 3.3 and later runs the guard as a PreDelete hook: deleting the Application while ManagedWorkloads are left is blocked, with a `DeletionError` condition, and Argo CD retries the hook until they're gone. To delete it anyway, set `guard.uninstall: false` in Git and sync first. Earlier versions of Argo CD don't run Helm's pre-delete hooks, so deleting the Application isn't guarded: stop managing the workloads before deleting it.

!!! warning
    Removing the operator or the CRD first skips step 2's restore: workloads that were paused stay at zero replicas, and their Services still send traffic to the doorman's deleted pods. Restore them by hand, as below.

### Restoring workloads after the operator is gone

If the operator was removed while ManagedWorkloads were left, with `helm uninstall --no-hooks`, `kubectl delete -f install.yaml`, or by deleting an Argo CD Application before Argo CD 3.3, nothing restores their workloads. Each ManagedWorkload also keeps its `hybernate.io/cleanup` finalizer, which only the operator removes, so deleting it, or the CRD, hangs in `Terminating`. Reinstalling the operator, then following the steps above, is the simplest fix. Without it:

1. Scale each paused workload back to the replicas it had, which its ManagedWorkload records:

    ```bash
    kubectl get managedworkloads -A -o jsonpath='{range .items[?(@.status.pause)]}{.metadata.namespace} {.spec.target.kind} {.spec.target.name} {.status.pause.previousReplicas}{"\n"}{end}' |
      while read -r ns kind name replicas; do
        kubectl scale "$kind/$name" -n "$ns" --replicas="$replicas"
      done
    ```

    A workload scaled by KEDA was held at zero through its ScaledObject, named in the ManagedWorkload's `status.pause.scaledObject`. Remove that ScaledObject's `autoscaling.keda.sh/paused-replicas` annotation, or set it back to `status.pause.scaledObjectPausedReplicas` if that's set.

    Each paused workload also carries the same record in its `hybernate.io/paused-from` annotation, which you can read when its ManagedWorkload is gone. Remove the annotation once the workload is restored:

    ```bash
    kubectl annotate deployments,statefulsets --all -A hybernate.io/paused-from-
    ```

2. Delete the EndpointSlices that route paused workloads' Services to the doorman, so the Services use their own pods again:

    ```bash
    kubectl delete endpointslices -A -l endpointslice.kubernetes.io/managed-by=doorman.hybernate.io
    ```

3. Remove the finalizers, so the ManagedWorkloads, and the CRD, can be deleted:

    ```bash
    kubectl get managedworkloads -A -o jsonpath='{range .items[*]}{.metadata.namespace} {.metadata.name}{"\n"}{end}' |
      while read -r ns name; do
        kubectl patch managedworkload "$name" -n "$ns" --type=merge -p '{"metadata":{"finalizers":null}}'
      done
    kubectl delete managedworkloads --all --all-namespaces
    kubectl delete crd managedworkloads.hybernate.io
    ```

## Next Steps

Follow the [Quickstart](quickstart.md) to manage your first workload.
