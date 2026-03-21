# API Reference

## ManagedWorkload

**Group:** `hybernate.io` | **Version:** `v1alpha1` | **Kind:** `ManagedWorkload` | **Scope:** Namespaced

### Spec (`ManagedWorkloadSpec`)

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `target` | `WorkloadRef` | Yes | | The workload to manage |
| `target.kind` | `Deployment` \| `StatefulSet` | Yes | `Deployment` | Target kind |
| `target.name` | string | Yes | | Target name (same namespace) |
| `desiredState` | `Running` \| `Paused` \| `Destroyed` | No | | Manual lifecycle override |
| `idlePolicy` | `IdlePolicySpec` | No | | Idle detection configuration |
| `idlePolicy.action` | `auto` \| `pause` \| `destroy` | No | `auto` | Action on idle confirmation |
| `idlePolicy.cpuIdleThreshold` | int | No | `50` | CPU millicores threshold |
| `idlePolicy.memoryIdleThreshold` | int64 | No | `104857600` | Memory bytes threshold (100Mi) |
| `idlePolicy.gracePeriod` | duration | No | | Continuous idle confirmation period |
| `idlePolicy.autoResume` | bool | No | `false` | Resume when signals clear |
| `idlePolicy.signals[]` | `ProbeSpec` | No | | Additional signal checks |
| `scalePolicy` | `ScalePolicySpec` | No | | Scaling configuration |
| `scalePolicy.minReplicas` | int | No | `1` | Minimum replicas |
| `scalePolicy.maxReplicas` | int | Yes | | Maximum replicas |
| `scalePolicy.overrideReplicas` | int | No | | Force specific replica count |
| `scalePolicy.down` | `ScaleDirectionSpec` | No | | Scale-down constraints |
| `scalePolicy.up` | `ScaleDirectionSpec` | No | | Scale-up constraints |
| `pause` | `PauseSpec` | No | | Pause behavior |
| `pause.expireAfter` | duration | No | | Max pause duration |
| `pause.expireAction` | `resume` \| `destroy` | No | `destroy` | Action on expiry |
| `destroy` | `DestroySpec` | No | | Destroy behavior |
| `destroy.pvcRetention` | duration | No | | PVC retention after destroy |
| `destroy.pvcRetentionWarning` | duration | No | | Warning before PVC cleanup |
| `prediction` | `PredictionSpec` | Yes | | Forecast engine config |
| `prediction.confidence` | int (0-100) | No | `85` | Confidence threshold |
| `costTracking` | `CostTrackingSpec` | No | | Custom cost rate overrides |
| `costTracking.rates` | `CostRates` | No | AWS defaults | Custom cost rates |
| `conflictAction` | `enforce` \| `warn` \| `defer` | No | `warn` | Drift handling |
| `dryRun` | bool | No | `false` | Evaluate without acting |

### ScaleDirectionSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `stabilization` | duration | No | Cooldown after same-direction scale |
| `maxStep` | int (>=1) | No | Max replicas to add/remove per reconcile |
| `guard[]` | `ProbeSpec` | No | Prometheus safety checks (scale-down only) |

### ProbeSpec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `source` | `prometheus` | No | `prometheus` | Signal source |
| `promQL` | string | No | | PromQL instant query |

### CostRates

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `cpuPerHour` | quantity | `0.031` | USD per vCPU-hour |
| `memoryPerHour` | quantity | `0.004` | USD per GiB-hour |
| `storagePerMonth` | quantity | `0.08` | USD per GiB-month |

### Status (`ManagedWorkloadStatus`)

| Field | Type | Description |
|-------|------|-------------|
| `phase` | `WorkloadPhase` | Current lifecycle phase |
| `conditions[]` | `Condition` | Standard K8s conditions |
| `pause` | `PauseStatus` | State while paused |
| `pause.previousReplicas` | int32 | Replicas before pause |
| `pause.pausedAt` | time | When paused |
| `pause.resources` | `ResourceSnapshot` | Resource profile at pause |
| `scale` | `ScaleStatus` | Last scaling event |
| `scale.previousReplicas` | int32 | Replicas before scale |
| `scale.currentReplicas` | int32 | Replicas after scale |
| `scale.scaledAt` | time | When scaled |
| `destroy` | `DestroyStatus` | State after destroy |
| `destroy.destroyedAt` | time | When destroyed |
| `destroy.resources` | `ResourceSnapshot` | Resource profile at destroy |
| `destroy.pvcRetentionExpiresAt` | time | When PVCs will be cleaned up |
| `prediction` | `PredictionStatus` | Forecast engine state |
| `prediction.dailyPhase` | string | Daily season phase |
| `prediction.dailyConfidence` | int | Daily accuracy % |
| `prediction.weeklyPhase` | string | Weekly season phase |
| `prediction.weeklyConfidence` | int | Weekly accuracy % |
| `cost` | `CostStatus` | Cost data |
| `cost.currentMonthCPUHours` | quantity | vCPU-hours this month |
| `cost.currentMonthMemoryHours` | quantity | GiB-hours this month |
| `cost.currentMonthStorageHours` | quantity | GiB-hours storage this month |
| `cost.estimatedMonthlyCost` | string | Projected monthly cost |
| `cost.estimatedMonthlySavings` | string | Estimated savings this month (requires autoscaler for realization) |
| `cost.estimatedCostWithoutManagement` | string | Estimated cost without Hybernate |
| `cost.resourceReduction` | `ResourceReduction` | Concrete resources freed by Hybernate actions |
| `cost.resourceReduction.cpuMillis` | int64 | CPU millicores freed |
| `cost.resourceReduction.memoryBytes` | int64 | Memory bytes freed |
| `cost.resourceReduction.replicas` | int32 | Pod replicas removed |
| `lastActedAt` | time | Last workload mutation |
| `lastTransitionTime` | time | Last phase change |

### WorkloadPhase Values

`Creating`, `Running`, `Idle`, `Scaling`, `Pausing`, `Paused`, `Resuming`, `Destroying`, `Destroyed`

---

## WorkloadPolicy

**Group:** `hybernate.io` | **Version:** `v1alpha1` | **Kind:** `WorkloadPolicy` | **Scope:** Namespaced | **Short name:** `wp`

### Spec (`WorkloadPolicySpec`)

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `targetKinds[]` | `TargetKind` | No | `[Deployment, StatefulSet]` | Kinds to scan |
| `mode` | `suggest` \| `auto-manage` | No | `suggest` | Operating mode |
| `scanInterval` | duration | No | `10m` | Re-scan frequency |
| `cpuIdleThreshold` | int | No | `50` | CPU millis for Idle classification |
| `memoryIdleThreshold` | int64 | No | `104857600` | Memory bytes for Idle classification (100Mi) |
| `cpuWastefulThreshold` | int (0-100) | No | `30` | CPU utilization % for Wasteful |
| `memoryWastefulThreshold` | int (0-100) | No | `30` | Memory utilization % for Wasteful |
| `rightSizeTarget` | int (1-100) | No | `70` | Target utilization for savings |
| `dryRun` | bool | No | `true` | Default for auto-created CRs |
| `rates` | `CostRates` | No | AWS defaults | Cost rates |
| `idlePolicy` | `IdlePolicySpec` | No | See defaults | Default idle policy |
| `scalePolicy` | `ScalePolicySpec` | No | See defaults | Default scale policy |
| `pause` | `PauseSpec` | No | See defaults | Default pause behavior |
| `destroy` | `DestroySpec` | No | See defaults | Default destroy behavior |
| `prediction` | `PredictionSpec` | No | `{confidence: 85}` | Default prediction config |
| `costTracking` | `CostTrackingSpec` | No | AWS defaults | Custom cost rate overrides |
| `conflictAction` | `ConflictAction` | No | `warn` | Default conflict handling |

### Status (`WorkloadPolicyStatus`)

| Field | Type | Description |
|-------|------|-------------|
| `summary.total` | int | Total workloads discovered |
| `summary.active` | int | Active workloads |
| `summary.idle` | int | Idle workloads |
| `summary.wasteful` | int | Wasteful workloads |
| `summary.managed` | int | Already-managed workloads |
| `summary.estimatedMonthlyCost` | string | Total estimated cost |
| `summary.estimatedPotentialSavings` | string | Total potential savings |
| `lastScanAt` | time | Last scan timestamp |
| `conditions[]` | `Condition` | Standard K8s conditions |
| `discovered[]` | `DiscoveredWorkload` | Per-workload results (max 500) |

---

## HybernateReport

**Group:** `hybernate.io` | **Version:** `v1alpha1` | **Kind:** `HybernateReport` | **Scope:** Cluster

A singleton resource that aggregates data across all ManagedWorkloads. The operator updates its status on each reconcile.

### Status

| Field | Type | Description |
|-------|------|-------------|
| `managed` | int | Total ManagedWorkloads |
| `active` | int | Running workloads |
| `paused` | int | Paused workloads |
| `destroyed` | int | Destroyed workloads |
| `totalCPUHours` | quantity | Aggregate CPU hours |
| `totalMemoryHours` | quantity | Aggregate memory hours |
| `totalStorageHours` | quantity | Aggregate storage hours |
| `estimatedMonthlyCost` | string | Total estimated cost |
| `estimatedTotalSavings` | string | Estimated total savings (requires autoscaler for realization) |
| `estimatedCostWithoutManagement` | string | Estimated total cost without Hybernate |
| `totalResourceReduction` | `ResourceReduction` | Aggregate resources freed across all workloads |
