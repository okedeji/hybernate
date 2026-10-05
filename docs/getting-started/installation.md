# Installation

## Prerequisites

- Kubernetes v1.26+ (tested in CI on 1.26 and 1.37)
- kubectl v1.26+
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

!!! warning
    Removing the operator or the CRD first skips step 2's restore: workloads that were paused stay at zero replicas, and you must scale them back up yourself.

## Next Steps

Follow the [Quickstart](quickstart.md) to manage your first workload.
