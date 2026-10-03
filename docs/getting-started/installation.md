# Installation

## Prerequisites

- Kubernetes cluster v1.26+
- kubectl v1.26+
- [metrics-server](https://github.com/kubernetes-sigs/metrics-server) installed (Hybernate reads pod CPU/memory via the Kubernetes Metrics API)
- Helm v3 (if using Helm install)

## Install

=== "Helm"

    Install directly from the OCI registry:

    ```bash
    helm install hybernate oci://ghcr.io/okedeji/charts/hybernate \
      --version v0.1.7 \
      --namespace hybernate-system \
      --create-namespace
    ```

    ??? tip "Helm Values"

        | Value | Default | Description |
        |-------|---------|-------------|
        | `replicaCount` | `1` | Number of operator replicas |
        | `image.repository` | `ghcr.io/okedeji/hybernate` | Container image |
        | `image.tag` | `latest` | Image tag |
        | `leaderElection.enabled` | `true` | Enable HA leader election |
        | `metrics.secure` | `true` | Serve metrics over HTTPS |
        | `defaults.idleAfter` | `1h` | Idle time before pausing, for opted-in workloads that don't set their own |
        | `defaults.cpuThreshold` | `10` | CPU percentage of requests that counts as active |
        | `defaults.dryRun` | `false` | Measure every opted-in workload without pausing, unless it sets its own |
        | `resources.limits.cpu` | `500m` | CPU limit |
        | `resources.limits.memory` | `128Mi` | Memory limit |

=== "kubectl"

    Apply the all-in-one installer manifest directly:

    ```bash     
    kubectl apply -f https://github.com/okedeji/hybernate/releases/latest/download/install.yaml
    ```

    This installs the CRDs, RBAC, and the operator Deployment into the `hybernate-system` namespace.

=== "Source"

    Clone the repo, install CRDs, then build and deploy the operator:

    ```bash
    git clone https://github.com/okedeji/hybernate.git
    cd hybernate

    # Install CRDs
    make install

    # Build and deploy the operator
    make docker-build IMG=ghcr.io/okedeji/hybernate:dev
    make deploy IMG=ghcr.io/okedeji/hybernate:dev
    ```

## Verify Installation

```bash
# Check the operator is running
kubectl get pods -n hybernate-system

# Verify CRDs are installed
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

3. Remove the operator and its CRDs:

    ```bash
    helm uninstall hybernate -n hybernate-system   # or: make undeploy && make uninstall
    ```

!!! warning
    Removing the operator or its CRDs first skips step 2's restore: workloads that were paused stay at zero replicas, and you must scale them back up yourself.

## Next Steps

Follow the [Quickstart](quickstart.md) to manage your first workload.
