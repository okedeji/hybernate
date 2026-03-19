# Cost Tracking

Hybernate tracks per-workload resource consumption and calculates the cost savings from its actions (pause, scale, destroy). Cost data is available per-workload in the ManagedWorkload status and aggregated cluster-wide in the HybernateReport.

!!! info "Cluster autoscaler required for node-level savings"
    Hybernate operates at the workload layer: it removes pods, freeing up node capacity. For that freed capacity to translate into real cost savings, your cluster needs an autoscaler (Cluster Autoscaler, Karpenter, or a managed equivalent like GKE Autopilot) that removes underutilized nodes. See the [Cluster Autoscaler Guide](../guides/cluster-autoscaler.md) for recommended settings.

## How Costs Are Calculated

Cost tracking is always enabled. Every ManagedWorkload accumulates resource consumption and calculates savings automatically using AWS on-demand defaults.

### Resource Accumulation

Every reconcile, Hybernate reads the workload's current resource usage and accumulates time-weighted consumption:

```
CPU Hours    += cpu_cores × elapsed_hours
Memory Hours += memory_gib × elapsed_hours
Storage Hours += storage_gib × elapsed_hours
```

Elapsed time is capped at 2 hours per accumulation to bound error after operator restarts.

### Dollar Costs

Resource hours are converted to dollar costs using configurable rates:

| Resource | Default Rate | Source |
|----------|-------------|--------|
| CPU | $0.031/vCPU-hour | AWS on-demand (m6i.large, us-east-1, 2026) |
| Memory | $0.004/GiB-hour | AWS on-demand (m6i.large, us-east-1, 2026) |
| Storage | $0.08/GiB-month ($0.000110/GiB-hour) | AWS EBS gp3, us-east-1 |

### Custom Rates

Override defaults to match your cloud provider's pricing:

```yaml title="managedworkload.yaml" linenums="1"
spec:
  costTracking:
    rates:
      cpuPerHour: "0.045"      # GKE Autopilot pricing
      memoryPerHour: "0.005"    # GKE Autopilot pricing
      storagePerMonth: "0.10"   # Premium SSD
```

## Savings Calculation

Savings are accumulated when Hybernate takes action:

- **Paused workloads**: CPU and memory savings accrue every reconcile while paused. Storage savings are zero (PVCs persist while paused).
- **Destroyed workloads**: CPU and memory savings accrue. Storage savings accrue only after PVC retention expires and PVCs are cleaned up.
- **Scaled-down workloads**: Savings from the difference between previous and current replica counts.

## Status Fields

Cost data appears in `status.cost`:

```yaml title="status.cost" linenums="1"
status:
  cost:
    currentMonthCPUHours: "720"
    currentMonthMemoryHours: "1440"
    currentMonthStorageHours: "7300"
    estimatedMonthlyCost: "$45.60"
    monthlySavings: "$23.40"
    costWithoutManagement: "$69.00"
    lastAccumulatedAt: "2026-03-18T14:30:00Z"
```

| Field | Description |
|-------|-------------|
| `estimatedMonthlyCost` | Projected full-month cost based on usage so far. Shows "pending" on day 1. |
| `monthlySavings` | Total saved by Hybernate this month (pause + scale + destroy). |
| `costWithoutManagement` | What it would have cost without Hybernate: estimated cost + savings. |

## Cluster-Wide Aggregation

The HybernateReport singleton aggregates cost data across all ManagedWorkloads:

```bash
kubectl get hybernatereport cluster-report -o jsonpath='{.status}'
```

This gives you total managed workload count, aggregate CPU/memory/storage hours, total estimated cost, and total savings, providing a single view of Hybernate's impact.

## Prometheus Metrics

Cost data is also exposed as Prometheus metrics:

- `hybernate_cost_estimated_dollars` (per workload)
- `hybernate_cost_savings_dollars` (per workload)
- `hybernate_cost_without_management_dollars` (per workload)
- `hybernate_cost_cpu_hours` (per workload)
- `hybernate_cost_memory_hours` (per workload)
- `hybernate_cost_storage_hours` (per workload)

See the [Metrics Reference](../reference/metrics.md) for the full list.
