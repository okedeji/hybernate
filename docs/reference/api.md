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
| `idlePolicy.action` | `pause` \| `destroy` | No | `pause` | Action once idle for `idleAfter` |
| `idlePolicy.idleAfter` | duration | No | `1h` | Time without activity before acting |
| `idlePolicy.activity.cpuThreshold` | int | No | `10` | CPU utilization % of requests above which the workload is active (0-100) |
| `idlePolicy.activity.prometheus[].promQL` | string | Yes (per entry) | | PromQL query; a result above zero is activity |
| `idlePolicy.autoResume` | bool | No | `false` | Wake ahead of confident forecast demand |
| `dependsOn[]` | `DependencyRef` | No | | Workloads this one needs |
| `dependsOn[].namespace` | string | No | this namespace | Namespace of the dependency |
| `dependsOn[].kind` | `Deployment` \| `StatefulSet` | Yes | | Kind of the dependency |
| `dependsOn[].name` | string | Yes | | Name of the dependency's workload |
| `dependsOn[].waitForReady` | bool | No | `false` | Hold this workload's resume until the dependency is Ready |
| `wake` | `WakeSpec` | No | | Waking on request while paused |
| `wake.onRequest` | bool | No | `true` | Route the workload's Services to the doorman while paused |
| `wake.maxWait` | duration | No | `2m` | How long a request is held while the workload wakes |
| `wake.page` | bool | No | `true` | Answer a browser loading a page with a waking-up page instead of holding it |
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
| `conflictAction` | `enforce` \| `warn` \| `defer` | No | `warn` | Handling of a paused workload scaled up externally |
| `dryRun` | bool | No | `false` | Evaluate without acting |

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
| `activity.lastActivityTime` | time | Most recent activity from any source |
| `activity.lastActivitySource` | string | `created`, `woke`, `request`, `cpu`, `rollout`, `annotation`, `prometheus`, or `unobserved` |
| `activity.pauseAt` | time | When the idle action runs if no further activity is seen |
| `activity.lastEvaluatedTime` | time | When activity was last checked |
| `activity.templateHash` | string | Fingerprint of the target's pod template, used to detect deploys |
| `dryRun` | `DryRunStatus` | What dry-run has measured; only while `spec.dryRun` is true |
| `dryRun.since` | time | When dry-run started measuring |
| `dryRun.pauses` | int32 | Times the workload would have been paused, one under way included |
| `dryRun.slept` | duration | How long finished would-be pauses lasted in all |
| `dryRun.estimatedSavings` | string | What the replicas freed during `slept` would have cost |
| `dryRun.resources` | `ResourceSnapshot` | What the workload ran when the current would-be pause began |
| `doorman[]` | `DoormanRoute` | Service ports routed to the doorman while paused |
| `doorman[].service` | string | Service name |
| `doorman[].portName` | string | Service port name |
| `doorman[].doormanPort` | int32 | Doorman port standing in for it |
| `pause.previousReplicas` | int32 | Replicas before pause |
| `pause.pausedAt` | time | When paused |
| `pause.resources` | `ResourceSnapshot` | Resource profile at pause |
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
| `cost.listRates` | `CostRates` | On-demand list rates of the nodes its pods last ran on, which it's priced at unless `costTracking.rates` sets its own. Unset when its nodes have no list price |
| `cost.resourceReduction` | `ResourceReduction` | Concrete resources freed by Hybernate actions |
| `cost.resourceReduction.cpuMillis` | int64 | CPU millicores freed |
| `cost.resourceReduction.memoryBytes` | int64 | Memory bytes freed |
| `cost.resourceReduction.replicas` | int32 | Pod replicas removed |
| `lastActedAt` | time | Last workload mutation |
| `lastTransitionTime` | time | Last phase change |

### WorkloadPhase Values

`Creating`, `Running`, `Idle`, `Pausing`, `Paused`, `Resuming`, `Destroying`, `Destroyed`
