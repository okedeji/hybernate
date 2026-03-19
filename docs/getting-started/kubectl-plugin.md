# kubectl Plugin

Hybernate provides a `kubectl hybernate` plugin for exporting discovered workloads as ManagedWorkload YAML manifests, designed for GitOps workflows.

## Installation

=== "Krew"

    ```bash
    kubectl krew install --manifest-url \
      https://github.com/okedeji/hybernate/releases/latest/download/krew-hybernate.yaml
    ```

    !!! note
        This uses a custom manifest URL. Once the plugin is accepted into the [krew-index](https://github.com/kubernetes-sigs/krew-index), you'll be able to install with just `kubectl krew install hybernate`.

=== "Binary"

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

=== "Source"

    ```bash
    git clone https://github.com/okedeji/hybernate.git
    cd hybernate
    make build-plugin
    # Binary is at bin/kubectl-hybernate
    ```

## Usage

!!! note
    The plugin requires a WorkloadPolicy to exist in the target namespace. It reads from the policy's `status.discovered` field. If the policy doesn't exist, the command exits with a clear error.

### Export All Unmanaged Workloads

```bash
kubectl hybernate export --policy staging-policy -n staging
```

Outputs YAML to stdout. Pipe to `kubectl apply` or redirect to a file:

```bash
kubectl hybernate export --policy staging-policy -n staging > manifests.yaml
```

### Export to Individual Files

```bash
kubectl hybernate export --policy staging-policy -n staging --output ./manifests/
```

Creates one file per workload (e.g., `manifests/my-api.yaml`).

### Filter by Classification

```bash
# Only idle workloads
kubectl hybernate export --policy staging-policy -n staging --classification Idle

# Only wasteful workloads
kubectl hybernate export --policy staging-policy -n staging --classification Wasteful

# Both
kubectl hybernate export --policy staging-policy -n staging \
  --classification Idle --classification Wasteful
```

### Export a Specific Workload

```bash
kubectl hybernate export --policy staging-policy -n staging --name my-api
```

### Include Already-Managed Workloads

By default, workloads that already have a ManagedWorkload CR are skipped. To include them (useful when graduating from auto-manage to GitOps):

```bash
kubectl hybernate export --policy staging-policy -n staging --include-managed
```

## Flags Reference

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--policy` | | _(required)_ | Name of the WorkloadPolicy to export from |
| `--namespace` | `-n` | `default` | Namespace of the WorkloadPolicy |
| `--output` | `-o` | _(stdout)_ | Directory to write individual YAML files |
| `--name` | | | Export only the workload with this name |
| `--classification` | | | Filter by classification (`Active`, `Idle`, `Wasteful`) |
| `--include-managed` | | `false` | Include workloads that already have a ManagedWorkload |

## GitOps Workflow

A typical workflow for introducing Hybernate via GitOps:

1. Deploy a WorkloadPolicy in `suggest` mode to discover workloads
2. Review discovered workloads: `kubectl get workloadpolicy staging-policy -n staging -o yaml`
3. Export the ones you want to manage: `kubectl hybernate export --policy staging-policy -n staging --classification Idle --output ./k8s/hybernate/`
4. Commit the manifests to your Git repository
5. Let ArgoCD/Flux sync them to the cluster

See the [GitOps Export Guide](../guides/gitops-export.md) for a detailed walkthrough.
