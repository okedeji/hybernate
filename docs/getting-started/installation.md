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
      --version v0.1.6 \
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
kubectl get crd workloadpolicies.hybernate.io
kubectl get crd hybernatereports.hybernate.io
```

## Uninstall

```bash
# Remove all CRs first
kubectl delete managedworkloads --all --all-namespaces
kubectl delete workloadpolicies --all --all-namespaces

# Remove the operator
make undeploy

# Remove CRDs
make uninstall
```

!!! warning
    Deleting CRDs removes all ManagedWorkload, WorkloadPolicy, and HybernateReport resources from the cluster. Workloads that were paused (scaled to zero) will remain at zero replicas. You must manually restore them.

## Next Steps

Follow the [Quickstart](quickstart.md) to manage your first workload.
