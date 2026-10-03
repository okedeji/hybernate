# kubectl Plugin

The `kubectl hybernate` plugin wakes paused workloads, and exports discovered workloads as ManagedWorkload manifests for GitOps workflows. Both use the namespace of your current kubeconfig context unless you pass `-n`, as kubectl does.

## Installation

=== "curl"

    Download the binary from the [releases page](https://github.com/okedeji/hybernate/releases) and place it in your `PATH`:

    ```bash
    # macOS (Apple Silicon)
    curl -LO https://github.com/okedeji/hybernate/releases/latest/download/kubectl-hybernate-darwin-arm64.tar.gz
    tar xzf kubectl-hybernate-darwin-arm64.tar.gz
    chmod +x kubectl-hybernate-darwin-arm64
    mv kubectl-hybernate-darwin-arm64 /usr/local/bin/kubectl-hybernate

    # Linux (amd64)
    curl -LO https://github.com/okedeji/hybernate/releases/latest/download/kubectl-hybernate-linux-amd64.tar.gz
    tar xzf kubectl-hybernate-linux-amd64.tar.gz
    chmod +x kubectl-hybernate-linux-amd64
    mv kubectl-hybernate-linux-amd64 /usr/local/bin/kubectl-hybernate
    ```

=== "Krew"

    ```bash
    kubectl krew install --manifest-url \
      https://github.com/okedeji/hybernate/releases/latest/download/krew-hybernate.yaml
    ```

    !!! note
        This uses a custom manifest URL. Once the plugin is accepted into the [krew-index](https://github.com/kubernetes-sigs/krew-index), you'll be able to install with just `kubectl krew install hybernate`.

=== "Source"

    ```bash
    git clone https://github.com/okedeji/hybernate.git
    cd hybernate
    make build-plugin
    # Binary is at bin/kubectl-hybernate
    ```

## Wake a Workload

```bash
kubectl hybernate wake my-api -n staging
```

```
waking staging/my-api...
staging/my-api is Running after 23s
```

`wake` marks the ManagedWorkload as active now by setting its `hybernate.io/last-activity` annotation, the same [activity annotation](../concepts/idle-detection.md#activity-annotations) a sandbox UI would set. A paused workload wakes, along with the workloads it [depends on](../concepts/dependencies.md); a running one has its idle clock restarted. It then waits until the workload is Running.

```bash
# Keep it awake for the next two hours, e.g. for a demo
kubectl hybernate wake my-api -n staging --for 2h

# Request the wake and return straight away
kubectl hybernate wake my-api -n staging --wait=false
```

It refuses, and says why, when activity can't wake the workload: `desiredState: Paused` (remove it or set it to `Running`), or a destroyed workload, whose Deployment or StatefulSet is gone.

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--namespace` | `-n` | kubeconfig context's | Namespace of the ManagedWorkload |
| `--for` | | | Also keep the workload awake for this long, by setting `hybernate.io/active-until` |
| `--wait` | | `true` | Wait until the workload is Running |
| `--timeout` | | `5m` | How long to wait |

Your user needs `get` and `patch` on `managedworkloads` in the namespace.

## Export Workloads

!!! note
    Export requires a WorkloadPolicy to exist in the target namespace. It reads from the policy's `status.discovered` field. If the policy doesn't exist, the command exits with a clear error.

### All Unmanaged Workloads

```bash
kubectl hybernate export --policy staging-policy -n staging
```

Outputs YAML to stdout. Pipe to `kubectl apply` or redirect to a file:

```bash
kubectl hybernate export --policy staging-policy -n staging > manifests.yaml
```

### To Individual Files

```bash
kubectl hybernate export --policy staging-policy -n staging --output ./manifests/
```

Creates one file per workload (e.g., `manifests/my-api.yaml`).

### Filter by Classification

```bash
# Only idle workloads
kubectl hybernate export --policy staging-policy -n staging --classification Idle
```

### A Specific Workload

```bash
kubectl hybernate export --policy staging-policy -n staging --name my-api
```

### Include Already-Managed Workloads

By default, workloads that already have a ManagedWorkload CR are skipped. To include them (useful when graduating from auto-manage to GitOps):

```bash
kubectl hybernate export --policy staging-policy -n staging --include-managed
```

### Export Flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--policy` | | _(required)_ | Name of the WorkloadPolicy to export from |
| `--namespace` | `-n` | kubeconfig context's | Namespace of the WorkloadPolicy |
| `--output` | `-o` | _(stdout)_ | Directory to write individual YAML files |
| `--name` | | | Export only the workload with this name |
| `--classification` | | | Filter by classification (`Active`, `Idle`) |
| `--include-managed` | | `false` | Include workloads that already have a ManagedWorkload |

## GitOps Workflow

A typical workflow for introducing Hybernate via GitOps:

1. Deploy a WorkloadPolicy in `suggest` mode to discover workloads
2. Review discovered workloads: `kubectl get workloadpolicy staging-policy -n staging -o yaml`
3. Export the ones you want to manage: `kubectl hybernate export --policy staging-policy -n staging --classification Idle --output ./k8s/hybernate/`
4. Commit the manifests to your Git repository
5. Let ArgoCD/Flux sync them to the cluster

See the [GitOps Export Guide](../guides/gitops-export.md) for a detailed walkthrough.
