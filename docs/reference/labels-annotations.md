# Labels & Annotations Reference

## Labels

| Label | Value | Applied To | Description |
|-------|-------|-----------|-------------|
| `hybernate.io/ignore` | `"true"` | Deployments, StatefulSets | Excludes the workload from discovery and auto-management. The workload still appears in `status.discovered` with `ignored: true` but is skipped by auto-manage and export. |
| `hybernate.io/auto-discovered` | `"true"` | ManagedWorkloads | Set on ManagedWorkloads created automatically by a WorkloadPolicy in `auto-manage` mode. Useful for filtering and identifying auto-created vs manually-created resources. |

### Usage

Exclude a workload from Hybernate:

```bash
kubectl label deployment my-critical-service hybernate.io/ignore=true
```

Find all auto-discovered ManagedWorkloads:

```bash
kubectl get managedworkloads -l hybernate.io/auto-discovered=true --all-namespaces
```

## Annotations

| Annotation | Value | Applied To | Description |
|------------|-------|-----------|-------------|
| `hybernate.io/workload-policy` | Policy name (string) | ManagedWorkloads | Links a ManagedWorkload back to the WorkloadPolicy that created or exported it. Set by auto-manage mode and the export plugin. |

### Usage

Find all ManagedWorkloads created by a specific policy:

```bash
kubectl get managedworkloads -n staging \
  -o jsonpath='{range .items[?(@.metadata.annotations.hybernate\.io/workload-policy=="staging-policy")]}{.metadata.name}{"\n"}{end}'
```

## Finalizers

| Finalizer | Applied To | Description |
|-----------|-----------|-------------|
| `hybernate.io/cleanup` | ManagedWorkloads | Added automatically by the operator. Ensures PVC retention cleanup completes before the ManagedWorkload CR can be deleted. The operator removes it after cleanup is done. |

!!! note
    Do not manually remove the `hybernate.io/cleanup` finalizer. If you do, PVCs scheduled for retention cleanup may be orphaned.
