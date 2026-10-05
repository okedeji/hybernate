# Cost Tracking

Hybernate tracks per-workload resource consumption and estimates the potential cost savings from pausing them. Cost data is available per-workload in the ManagedWorkload status.

!!! warning "Estimated savings vs. actual savings"
    Hybernate operates at the **workload layer** — it removes pods, freeing CPU and memory on nodes. But your cloud bill is based on **nodes**, not pods. Estimated savings are only realized when freed resources lead to node removal by a cluster autoscaler. If freed capacity isn't enough to drain a node, the node stays and no money is saved. Hybernate reports two things separately: **resource reduction** (always accurate — the concrete CPU/memory freed) and **estimated cost savings** (projected — assumes freed resources lead to node removal).

!!! info "Cluster autoscaler required for node-level savings"
    Hybernate operates at the workload layer: it removes pods, freeing up node capacity. For that freed capacity to translate into real cost savings, your cluster needs an autoscaler (Cluster Autoscaler, Karpenter, or a managed equivalent like GKE Autopilot) that removes underutilized nodes. See the [Cluster Autoscaler Guide](../guides/cluster-autoscaler.md) for recommended settings.

## How Costs Are Calculated

Cost tracking is always enabled. Every ManagedWorkload accumulates resource consumption and calculates savings automatically, priced at the on-demand list price of the nodes it runs on.

### Resource Accumulation

Every reconcile, Hybernate reads the workload's current resource usage, across every container in its pods including sidecars, and accumulates time-weighted consumption:

```
CPU Hours    += cpu_cores × elapsed_hours
Memory Hours += memory_gib × elapsed_hours
Storage Hours += storage_gib × elapsed_hours
```

Elapsed time is capped at 2 hours per accumulation to bound error after operator restarts.

### Prices

Resource hours are converted to dollars at rates per vCPU-hour and per GiB-hour. The first of these that applies sets them:

1. **Your own rates**, in `costTracking.rates`, each part on its own.
2. **The list price of its nodes.** Hybernate reads each pod's node's instance type and region from its labels (`node.kubernetes.io/instance-type`, `topology.kubernetes.io/region`), looks up its on-demand list price, and averages over the workload's pods. The rates are kept in `status.cost.listRates`, and while the workload is paused its savings stay priced at the nodes it last ran on.
3. **The defaults**, for nodes Hybernate has no list price for, such as on-premises, custom, or kind nodes:

| Resource | Default Rate | Source |
|----------|-------------|--------|
| CPU | $0.031/vCPU-hour | AWS on-demand (m6i.large, us-east-1, 2026) |
| Memory | $0.004/GiB-hour | AWS on-demand (m6i.large, us-east-1, 2026) |
| Storage | $0.08/GiB-month ($0.000110/GiB-hour) | AWS EBS gp3, us-east-1 |

**How a node's price becomes rates.** A cloud provider prices an instance as a whole, so Hybernate splits its hourly price between its vCPUs and its memory in the defaults' proportion: a vCPU costs as much as 7.75 GiB of memory. An `m6i.large` (2 vCPU, 8 GiB, $0.096 an hour) comes out at $0.032 per vCPU-hour and $0.0041 per GiB-hour. A memory-optimized instance's memory and a compute-optimized one's CPU come out at what they cost there, and a pod pays for the share of the node it requests.

**Where the prices come from.** Hybernate ships with the on-demand Linux list price of every AWS, Google Cloud, and Azure instance type, in every region it's sold, taken from the providers' public price lists through [instances.vantage.sh](https://instances.vantage.sh) and refreshed each release. Nothing is looked up at runtime, so it works offline and air-gapped. Instances on GPU nodes are priced the same way, so a pod on a GPU node pays its share of the GPU too.

**What list prices leave out.** Spot and preemptible nodes are priced at on-demand, so their workloads cost less than shown. Savings plans, reserved instances, committed-use discounts, and negotiated rates aren't known to Hybernate; set `costTracking.rates` for those.

Hybernate reads Nodes' metadata to do this: only their names and labels are cached.

### Custom Rates

Set your own rates to match what you pay:

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

- CPU and memory savings accrue every reconcile while a workload is paused. Storage isn't saved: its PVCs stay while it's paused.

Savings are priced on the CPU and memory a running pod requests, read just before the pause. That includes sidecars injected when the pod was created, such as a service mesh proxy, which the Deployment's own template doesn't list but which pausing frees too.

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
    listRates:
      cpuPerHour: 31661u      # $0.031661 per vCPU-hour
      memoryPerHour: 4085u    # $0.004085 per GiB-hour
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
| `listRates` | The on-demand list rates of the nodes the workload's pods last ran on, which it's priced at unless `costTracking.rates` sets its own. Unset when its nodes have no list price, and the defaults apply. |
| `resourceReduction` | Concrete CPU, memory, and replicas freed by Hybernate actions. Always accurate regardless of autoscaler behavior. |

## Viewing Costs Across Workloads

List cost and savings for every ManagedWorkload in the cluster:

```bash
kubectl get managedworkloads -A -o custom-columns=\
NAMESPACE:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase,\
COST:.status.cost.estimatedMonthlyCost,SAVINGS:.status.cost.estimatedMonthlySavings
```

Cost data is not exported as Prometheus metrics. The operator's metrics cover its own health; see the [Metrics Reference](../reference/metrics.md).
