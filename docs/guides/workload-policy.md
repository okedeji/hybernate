# WorkloadPolicy Guide

WorkloadPolicy automates workload discovery and classification within a namespace. It scans for Deployments and StatefulSets, fetches their metrics, and classifies each as Active, Idle, or Wasteful.

## Quick Start

```yaml title="workloadpolicy.yaml" linenums="1"
apiVersion: hybernate.io/v1alpha1
kind: WorkloadPolicy
metadata:
  name: staging-policy
  namespace: staging
spec:
  mode: suggest
  scanInterval: "10m"
  cpuIdleThreshold: 50
  cpuWastefulThreshold: 30
  rightSizeTarget: 70
```

```bash
kubectl apply -f workloadpolicy.yaml
```

After the first scan, check results:

```bash
kubectl get workloadpolicy staging-policy -n staging
```

Output:

```
NAME             MODE      DISCOVERED   ACTIVE   IDLE   WASTEFUL   PROJECTED COST   PROJECTED SAVINGS
staging-policy   suggest   12           8        2      2          $340.00          $89.00
```

## Modes

### `suggest` (default)

Discovery only. The policy scans and classifies workloads, populates `status.discovered`, and reports summary metrics. No ManagedWorkloads are created.

Use this mode to:

- Audit a namespace for cost optimization opportunities
- Review classifications before enabling management
- Feed the `kubectl hybernate export` plugin

### `auto-manage`

Automatically creates ManagedWorkload CRs for idle and wasteful workloads using the policy's default settings. Active workloads are left alone.

```yaml title="workloadpolicy.yaml" linenums="1"
spec:
  mode: auto-manage
  dryRun: true  # Auto-created ManagedWorkloads start in dry-run mode
```

!!! warning
    Start with `dryRun: true` when using `auto-manage`. This creates the ManagedWorkloads but they won't take action until you set `dryRun: false` on each one individually.

## Classification Thresholds

| Classification | Condition |
|---------------|-----------|
| **Idle** | CPU usage < `cpuIdleThreshold` AND memory usage < `memoryIdleThreshold` |
| **Wasteful** | CPU utilization < `cpuWastefulThreshold` OR memory utilization < `memoryWastefulThreshold` |
| **Active** | Everything else |

**Utilization** is calculated as `(usage / request) x 100%`. A workload requesting 1000m CPU but using 200m has 20% utilization, which is classified as Wasteful.

**Right-size savings** are estimated as the cost difference between current resources and what would be needed at `rightSizeTarget` utilization (default: 70%).

## Default Policies

WorkloadPolicy sets defaults for ManagedWorkloads it creates (in auto-manage mode) or exports (via kubectl plugin):

```yaml title="workloadpolicy.yaml" linenums="1"
spec:
  idlePolicy:
    action: pause
    cpuIdleThreshold: 50
    gracePeriod: "5m"
    autoResume: true

  scalePolicy:
    minReplicas: 1
    maxReplicas: 10
    down:
      stabilization: "5m"
    up:
      stabilization: "2m"

  pause:
    expireAfter: "168h"
    expireAction: Resume

  destroy:
    pvcRetention: "168h"
    pvcRetentionWarning: "24h"

  prediction:
    confidence: 85

  conflictAction: warn
```

These are all configurable. They serve as sensible defaults that you can override per-workload.

## Excluding Workloads

To prevent a workload from being discovered, add the ignore label:

```bash
kubectl label deployment my-critical-service hybernate.io/ignore=true -n staging
```

Ignored workloads appear in `status.discovered` with `ignored: true` but are skipped by auto-manage and export.

## Viewing Discovered Workloads

Full details per workload:

```bash
kubectl get workloadpolicy staging-policy -n staging -o jsonpath='{.status.discovered}' | jq .
```

Each entry includes:

- `name`, `kind`, `classification`
- `cpuUsageMillis`, `cpuRequestMillis`, `utilizationPercent`
- `memoryUsageBytes`, `memoryRequestBytes`
- `storageBytes`
- `estimatedMonthlyCost`, `estimatedPotentialSavings`
- `managed` (already has a ManagedWorkload)
- `ignored` (has the `hybernate.io/ignore` label)

Results are capped at 500 entries, sorted by estimated savings descending.

## Spec Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `targetKinds` | list | `[Deployment, StatefulSet]` | Which kinds to scan |
| `mode` | `suggest` or `auto-manage` | `suggest` | Reporting only or auto-create ManagedWorkloads |
| `scanInterval` | duration | `10m` | How often to re-scan |
| `cpuIdleThreshold` | int (millicores) | `50` | CPU below this = Idle |
| `memoryIdleThreshold` | int64 (bytes) | `104857600` (100Mi) | Memory below this = Idle |
| `cpuWastefulThreshold` | int (percent) | `30` | CPU utilization below this = Wasteful |
| `memoryWastefulThreshold` | int (percent) | `30` | Memory utilization below this = Wasteful |
| `rightSizeTarget` | int (percent) | `70` | Target utilization for savings estimates |
| `dryRun` | bool | `true` | Default dryRun for auto-created ManagedWorkloads |
| `rates` | CostRates | AWS defaults | Cost rates for savings estimates |
