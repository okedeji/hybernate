# Cost Tracking

Hybernate tracks what each workload costs, and what pausing it saves, in its ManagedWorkload's `status.cost`. The figures cover the current calendar month in UTC.

!!! warning "Saved is not the same as off your bill"
    Hybernate works at the **workload layer**: pausing removes pods, which frees the CPU and memory they requested on their nodes. Your cloud bill is for **nodes**. What pausing saves becomes money only when a cluster autoscaler (Cluster Autoscaler, Karpenter, or a managed equivalent such as GKE Autopilot) removes the capacity it frees; if the freed capacity isn't enough to drain a node, the node stays and nothing is saved. Hybernate reports **resource reduction**, the CPU and memory a pause frees, apart from **savings**, those resources priced. See the [Cluster Autoscaler Guide](../guides/cluster-autoscaler.md) for settings that let freed capacity go.

## How Costs Are Calculated

Cost tracking is always on. Everything is priced on what the workload's pods **request**, not on what they use: requests are what a pod reserves on a node, so they're what the workload costs in capacity, and what a cluster autoscaler can remove once it's paused. That includes sidecars injected when the pods were created, such as a service mesh proxy, which the Deployment's own template doesn't list but which pausing frees too.

### Resource Hours

Hybernate keeps the month's totals as resource-hours, brought up to date at every phase change, so time counts in the phase it was spent in, and at least every 5 minutes in between:

| Field | What it counts |
|-------|----------------|
| `awakeCPUHours`, `awakeMemoryHours` | vCPU-hours and GiB-hours the workload's replicas requested while it was awake: in any phase but `Paused` |
| `storageHours` | GiB-hours of the PersistentVolumeClaims its pods mount, in every phase: a pause doesn't free storage |
| `pausedCPUHours`, `pausedMemoryHours` | vCPU-hours and GiB-hours the replicas Hybernate paused had requested, for as long as they were paused: what pausing freed |

While the workload is awake, its replicas are read from the target each time, and what one replica requests is read from its pods hourly and whenever its pod template changes. While it's paused, the paused hours are priced on what it ran when the pause began (`status.pause.resources`).

A gap between updates longer than 2 hours, such as while the operator wasn't running, counts as 2 hours, since what the workload did meanwhile isn't known. `tracked` is how much of the month the totals cover: less than the time since the month began when the workload was created during the month, or after such a gap.

The hours are kept to a billionth of an hour, as Kubernetes quantities, so the short pauses of small workloads still add up: `83333333n` is 0.083 vCPU-hours. When a new month begins, the totals start again from zero; last month's aren't kept.

### Prices

Resource-hours are turned into dollars at a rate per vCPU-hour, per GiB-hour of memory, and per GiB-month of storage. For each of the three, the first of these that sets it applies:

1. **Your own rates**, in `spec.costTracking.rates`.
2. **The list price of its nodes**, in `status.cost.listRates`. Hybernate reads each pod's node's instance type and region from its labels (`node.kubernetes.io/instance-type`, `topology.kubernetes.io/region`), looks up its on-demand list price, and averages over the workload's pods. These are read again hourly while the workload is awake, and kept while it's paused, so its savings are priced at the nodes it last ran on. They set CPU and memory only.
3. **The defaults**, for nodes Hybernate has no list price for, such as on-premises, custom, or kind nodes:

| Resource | Default Rate | Source |
|----------|-------------|--------|
| CPU | $0.031/vCPU-hour | AWS on-demand (m6i.large, us-east-1, 2026) |
| Memory | $0.004/GiB-hour | AWS on-demand (m6i.large, us-east-1, 2026) |
| Storage | $0.08/GiB-month (priced at 730 hours a month) | AWS EBS gp3, us-east-1 |

The rates are applied to the whole month's hours each time the dollar figures are worked out, so changing `costTracking.rates` reprices the month so far, not only what comes after.

**How a node's price becomes rates.** A cloud provider prices an instance as a whole, so Hybernate splits its hourly price between its vCPUs and its memory in the defaults' proportion: a vCPU costs as much as 7.75 GiB of memory. An `m6i.large` (2 vCPU, 8 GiB, $0.096 an hour) comes out at $0.0317 per vCPU-hour and $0.0041 per GiB-hour. A memory-optimized instance's memory and a compute-optimized one's CPU come out at what they cost there, and a pod pays for the share of the node it requests.

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

## Status Fields

```yaml title="status.cost" linenums="1"
status:
  cost:
    tracked: 412h30m0s
    lastAccumulatedAt: "2026-10-18T14:30:00Z"
    awakeCPUHours: "160500m"
    awakeMemoryHours: "321"
    storageHours: "4125"
    pausedCPUHours: "664500m"
    pausedMemoryHours: "1329"
    costThisMonth: "$6.71"
    savedThisMonth: "$25.92"
    costWithoutHybernateThisMonth: "$32.63"
    projectedMonthlyCost: "$12.11"
    projectedMonthlySavings: "$46.71"
    running:
      replicas: 2
      cpuMillis: 500
      memoryBytes: 1073741824
      storageBytes: 10737418240
    pricedAt: "2026-10-18T14:00:00Z"
    pricedTemplateHash: 3f9c2a71d0e4b815
    listRates:
      cpuPerHour: 31660u      # $0.031660 per vCPU-hour
      memoryPerHour: 4085u    # $0.004085 per GiB-hour
```

| Field | Description |
|-------|-------------|
| `tracked` | How much of the month the totals cover |
| `lastAccumulatedAt` | When the totals were last brought up to date |
| `awakeCPUHours`, `awakeMemoryHours`, `storageHours`, `pausedCPUHours`, `pausedMemoryHours` | The resource-hours above: the record the dollar figures are worked out from |
| `costThisMonth` | The awake hours and the storage hours, priced |
| `savedThisMonth` | The paused hours, priced. Money only once a cluster autoscaler removes the capacity a pause frees. Also the `Saved` column of `kubectl get managedworkloads` |
| `costWithoutHybernateThisMonth` | What the workload would have cost this month had it never been paused: `costThisMonth` plus `savedThisMonth`, to within a cent |
| `projectedMonthlyCost`, `projectedMonthlySavings` | `costThisMonth` and `savedThisMonth` carried to the whole month at the same rate: the amount ÷ `tracked` × the hours in the month. `pending` until a day has been tracked, including on the first day of each month, since a workload's pattern is daily |
| `running` | What the workload ran when the totals were last brought up to date while it was awake |
| `pricedAt`, `pricedTemplateHash` | When what a replica requests, and `listRates`, were last read, and the pod template's hash then |
| `listRates` | The on-demand list rates of the nodes the workload's pods last ran on. Unset when its nodes aren't in Hybernate's price table, and the defaults apply |
| `resourceReduction` | What the current pause freed; only while the workload is paused |

The dollar figures are rounded to the cent for display; the hours are the record.

## Resource Reduction

While a workload is paused, `resourceReduction` says what the pause freed, which, unlike the dollar figures, doesn't depend on a cluster autoscaler removing nodes:

```yaml title="status.cost.resourceReduction" linenums="1"
resourceReduction:
  cpuMillis: 3000            # 3 vCPUs freed
  memoryBytes: 6442450944    # 6 GiB freed
  replicas: 3                # 3 pod replicas removed
```

Whether that becomes money depends on whether your cluster autoscaler can consolidate the remaining pods and remove a node.

## In Dry-Run

A workload in [dry-run](../guides/dry-run.md) is never paused, so all its time is awake time. What pausing would have freed is kept apart, in `status.dryRun`: `freedCPUHours`, `freedMemoryHours`, and `estimatedSavings`, those hours priced at the same rates.

## Viewing Costs Across Workloads

`kubectl get managedworkloads -A` shows each workload's `Saved` this month. For more:

```bash
kubectl get managedworkloads -A -o custom-columns=\
NAMESPACE:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase,\
COST:.status.cost.costThisMonth,SAVED:.status.cost.savedThisMonth,\
PROJECTED:.status.cost.projectedMonthlyCost
```

[`kubectl hybernate status`](../getting-started/kubectl-plugin.md#see-what-hybernate-is-doing) sums savings across the cluster.

Cost data is not exported as Prometheus metrics. The operator's metrics cover its own health; see the [Metrics Reference](../reference/metrics.md).
