# Labels & Annotations Reference

## Labels

| Label | Value | Applied To | Description |
|-------|-------|-----------|-------------|
| `hybernate.io/ignore` | `"true"` | Deployments, StatefulSets | Excludes the workload from discovery and auto-management. The workload still appears in `status.discovered` with `ignored: true` but is skipped by auto-manage and export. |
| `hybernate.io/managed-workload` | ManagedWorkload name | EndpointSlices | Set on the EndpointSlices that route a paused workload's Services to the doorman, along with `endpointslice.kubernetes.io/managed-by: doorman.hybernate.io`. They're removed when the workload is Running. |
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
| `hybernate.io/last-activity` | RFC 3339 time | ManagedWorkloads, Deployments, StatefulSets | Activity seen by another tool, such as a sandbox UI. A time after the pause wakes a paused workload. See [Activity annotations](../concepts/idle-detection.md#activity-annotations). |
| `hybernate.io/active-until` | RFC 3339 time | ManagedWorkloads, Deployments, StatefulSets | Keeps the workload awake until that time, and wakes it if paused. |
| `hybernate.io/last-request` | RFC 3339 time | ManagedWorkloads | Set by the doorman when it holds a request for a paused workload. Wakes it like `last-activity`, and the clock records the wake as `request`. See [Wake on Request](../concepts/wake-on-request.md). |

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
