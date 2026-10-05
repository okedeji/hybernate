# API Reference

## ManagedWorkload

**Group:** `hybernate.io` | **Version:** `v1alpha1` | **Kind:** `ManagedWorkload` | **Scope:** Namespaced

`kubectl get managedworkloads` shows each one's `Phase`, what it has `Saved` this month, and its `Age`.

### Spec (`ManagedWorkloadSpec`)

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `target` | `WorkloadRef` | Yes | | The workload to manage. Can't be changed once set |
| `target.kind` | `Deployment` \| `StatefulSet` | Yes | `Deployment` | Target kind |
| `target.name` | string | Yes | | Target name (same namespace) |
| `desiredState` | `Running` \| `Paused` | No | | Manual lifecycle override |
| `idlePolicy` | `IdlePolicySpec` | No | | Idle detection configuration. Without it, the workload is never paused automatically |
| `idlePolicy.idleAfter` | duration | No | `1h` | Time without activity before pausing |
| `idlePolicy.activity.cpuThreshold` | int | No | `10` | CPU utilization % of requests above which the workload is active (1-100) |
| `idlePolicy.activity.prometheus[].promQL` | string | Yes (per entry) | | PromQL instant query; a result above zero is activity |
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
| `prediction` | `PredictionSpec` | Yes | | Forecast engine config; `{}` takes the defaults |
| `prediction.confidence` | int (50-100) | No | `85` | Accuracy (1 − WAPE, %) a season's forecasts must reach before they drive decisions |
| `costTracking` | `CostTrackingSpec` | No | | Custom cost rate overrides |
| `costTracking.rates` | `CostRates` | No | node list prices, then AWS defaults | Custom cost rates |
| `dryRun` | bool | No | `false` | Evaluate without pausing |

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
| `conditions[]` | `Condition` | Standard Kubernetes conditions; see [Lifecycle](../concepts/lifecycle.md#status-conditions) |
| `lastActedAt` | time | When the operator last paused or resumed the target |
| `lastTransitionTime` | time | Last phase change |
| `activity.lastActivityTime` | time | Most recent activity from any source |
| `activity.lastActivitySource` | string | `created`, `woke`, `request`, `cpu`, `rollout`, `annotation`, `prometheus`, `unobserved`, or `scaled-up` |
| `activity.pauseAt` | time | When the workload pauses if no further activity is seen |
| `activity.lastEvaluatedTime` | time | When activity was last checked |
| `activity.templateHash` | string | Fingerprint of the target's pod template, used to detect deploys |
| `pause` | `PauseStatus` | What a pause changed, from before the workload is scaled to zero until it's running again |
| `pause.previousReplicas` | int32 | Replicas before the pause, restored on resume |
| `pause.pausedAt` | time | When it was scaled to zero; unset while the pause is under way |
| `pause.scaledObject` | string | The KEDA ScaledObject held at zero while paused; see [HPA and KEDA](../guides/autoscalers.md) |
| `pause.scaledObjectPausedReplicas` | string | The ScaledObject's own `autoscaling.keda.sh/paused-replicas` from before the pause, put back after it |
| `pause.resources` | `ResourceSnapshot` | What the workload ran when the pause began: `replicas`, and per replica `cpuMillis`, `memoryBytes`, and its claims' `storageBytes` |
| `pause.wakeAnnotations` | object | The activity annotations on the ManagedWorkload (`workload`) and its target (`target`) as the pause began; one that changes since wakes it |
| `learnedDependencies` | `LearnedDependencies` | Dependencies Hybernate found, held and woken like `dependsOn`: `from`, `at`, and `dependencies` (`namespace`, `kind`, `name`, `source`: `environment` or `wake`, and `via` and `address` for one found in the environment). See [Learned dependencies](../concepts/dependencies.md#learned-dependencies) |
| `lastScaledUp` | `ScaledUp` | The last time the workload was scaled up outside Hybernate while paused, which wakes it: `at`, `replicas`, `by` (the field manager, such as `kubectl-scale`), and `gitOps` (`Argo CD` or `Flux`, when it was set from Git; see [Argo CD and Flux](../guides/gitops.md)) |
| `doorman[]` | `DoormanRoute` | Service ports routed to the doorman while paused: `service`, `portName`, `doormanPort` |
| `dryRun` | `DryRunStatus` | What dry-run has measured; only while `spec.dryRun` is true |
| `dryRun.since` | time | When dry-run started measuring |
| `dryRun.pauses` | int32 | Times the workload would have been paused, one under way included |
| `dryRun.slept` | duration | How long finished would-be pauses lasted in all |
| `dryRun.freedCPUHours`, `dryRun.freedMemoryHours` | quantity | vCPU-hours and GiB-hours the replicas requested during `slept` |
| `dryRun.estimatedSavings` | string | Those hours priced, for display |
| `dryRun.resources` | `ResourceSnapshot` | What the workload ran when the current would-be pause began |
| `prediction.dailyPhase`, `prediction.weeklyPhase` | string | Each season's phase: `Observing`, `Suggesting`, or `Active` |
| `prediction.dailyConfidence`, `prediction.weeklyConfidence` | int | Accuracy % over the last 24 and 168 observed hours |
| `prediction.state` | string | The engine's learned state, gzipped and base64-encoded |
| `cost` | `CostStatus` | Cost for the current calendar month, in UTC; see [Cost Tracking](../concepts/cost-tracking.md#status-fields) |
| `cost.tracked` | duration | How much of the month the totals cover |
| `cost.lastAccumulatedAt` | time | When the totals were last brought up to date |
| `cost.awakeCPUHours`, `cost.awakeMemoryHours` | quantity | vCPU-hours and GiB-hours requested while awake |
| `cost.storageHours` | quantity | GiB-hours of the claims its pods mount, in every phase |
| `cost.pausedCPUHours`, `cost.pausedMemoryHours` | quantity | vCPU-hours and GiB-hours the paused replicas had requested, while paused |
| `cost.costThisMonth` | string | Awake and storage hours, priced |
| `cost.savedThisMonth` | string | Paused hours, priced |
| `cost.costWithoutHybernateThisMonth` | string | `costThisMonth` plus `savedThisMonth` |
| `cost.projectedMonthlyCost`, `cost.projectedMonthlySavings` | string | The month so far carried to the whole month; `pending` until a day is tracked |
| `cost.running` | `ResourceSnapshot` | What the workload ran when last brought up to date while awake |
| `cost.pricedAt`, `cost.pricedTemplateHash` | time, string | When requests and list rates were last read, and the pod template then |
| `cost.listRates` | `CostRates` | On-demand list rates of the nodes its pods last ran on. Unset when its nodes have no list price |
| `cost.resourceReduction` | `ResourceReduction` | What the current pause freed: `cpuMillis`, `memoryBytes`, `replicas`. Only while paused |

### WorkloadPhase Values

`Running`, `Idle`, `Pausing`, `Paused`, `Resuming`. A ManagedWorkload has no phase until its first reconcile. `Creating` is accepted by the schema but never set.
