# Cost Tracking

Hybernate tracks per-workload resource consumption and estimates the potential cost savings from its actions (pause, scale, destroy). Cost data is available per-workload in the ManagedWorkload status and aggregated cluster-wide in the HybernateReport.

!!! warning "Estimated savings vs. actual savings"
    Hybernate operates at the **workload layer** — it removes pods, freeing CPU and memory on nodes. But your cloud bill is based on **nodes**, not pods. Estimated savings are only realized when freed resources lead to node removal by a cluster autoscaler. If freed capacity isn't enough to drain a node, the node stays and no money is saved. Hybernate reports two things separately: **resource reduction** (always accurate — the concrete CPU/memory freed) and **estimated cost savings** (projected — assumes freed resources lead to node removal).

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

## Estimated Savings

Estimated savings are accumulated when Hybernate takes action. These projections assume that freed resources eventually lead to node removal by a cluster autoscaler:

- **Paused workloads**: CPU and memory savings accrue every reconcile while paused. Storage savings are zero (PVCs persist while paused).
- **Destroyed workloads**: CPU and memory savings accrue. Storage savings accrue only after PVC retention expires and PVCs are cleaned up.
- **Scaled-down workloads**: Savings from the difference between previous and current replica counts.

## Resource Reduction

Unlike estimated savings, resource reduction is always accurate. It tracks the concrete CPU and memory freed by removing pods:

```yaml title="status.cost.resourceReduction" linenums="1"
resourceReduction:
  cpuMillis: 3000      # 3 vCPUs freed
  memoryBytes: 6442450944  # 6 GiB freed
  replicas: 3          # 3 pod replicas removed
```

This tells you exactly what Hybernate freed at the workload level. Whether that translates to cost savings depends on whether your cluster autoscaler can consolidate remaining workloads and remove the freed node.

## Status Fields

Cost data appears in `status.cost`:

```yaml title="status.cost" linenums="1"
status:
  cost:
    currentMonthCPUHours: "720"
    currentMonthMemoryHours: "1440"
    currentMonthStorageHours: "7300"
    estimatedMonthlyCost: "$45.60"
    estimatedMonthlySavings: "$23.40"
    estimatedCostWithoutManagement: "$69.00"
    resourceReduction:
      cpuMillis: 3000
      memoryBytes: 6442450944
      replicas: 3
    lastAccumulatedAt: "2026-03-18T14:30:00Z"
```

| Field | Description |
|-------|-------------|
| `estimatedMonthlyCost` | Projected full-month cost based on usage so far. Shows "pending" on day 1. |
| `estimatedMonthlySavings` | Projected savings from Hybernate actions this month. Only realized when freed resources lead to node removal. |
| `estimatedCostWithoutManagement` | Estimated cost without Hybernate: estimated cost + estimated savings. |
| `resourceReduction` | Concrete CPU, memory, and replicas freed by Hybernate actions. Always accurate regardless of autoscaler behavior. |

## Cluster-Wide Aggregation

The HybernateReport singleton aggregates cost data across all ManagedWorkloads:

```bash
kubectl get hybernatereport cluster-report -o jsonpath='{.status}'
```

This gives you total managed workload count, aggregate CPU/memory/storage hours, total estimated cost, and total savings, providing a single view of Hybernate's impact.

## Prometheus Metrics

Cost data is also exposed as Prometheus metrics:

- `hybernate_cost_estimated_dollars` — estimated monthly cost
- `hybernate_cost_estimated_savings_dollars` — estimated monthly savings (requires autoscaler for realization)
- `hybernate_cost_estimated_without_management_dollars` — estimated cost without Hybernate
- `hybernate_resource_reduction_cpu_millicores` — total CPU millicores freed
- `hybernate_resource_reduction_memory_bytes` — total memory bytes freed
- `hybernate_cost_cpu_hours` — total vCPU-hours consumed
- `hybernate_cost_memory_hours` — total GiB-hours consumed
- `hybernate_cost_storage_hours` — total GiB-hours storage

See the [Metrics Reference](../reference/metrics.md) for the full list.
