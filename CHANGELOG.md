# Changelog

All notable changes to Hybernate will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Workload dependencies: `spec.dependsOn` lists the workloads a ManagedWorkload needs, in its own or another namespace. A dependency isn't paused while anything that depends on it is awake (`HeldByDependents`), waking a workload wakes its dependencies, and `waitForReady: true` holds a resume until a dependency's pods are Ready (`WaitingForDependencies`). Cycles stop both workloads from pausing (`DependencyCycle`); a dependency that doesn't exist is reported (`DependencyNotFound`) but doesn't block
- Prometheus activity sources: `idlePolicy.activity.prometheus` queries count as activity when they return a value above zero, such as an ingress request rate. Configure the endpoint with `--prometheus-url` (Helm value `prometheus.url`); path-prefixed endpoints (Thanos, Mimir, reverse proxies) work as-is. If a query can't be evaluated, the `PrometheusAvailable` condition reports `EndpointNotConfigured` or `QueryFailed` and the workload isn't paused
- `MetricsAvailable` reason `NoCPURequests`: idle detection doesn't act on a workload whose CPU utilization can't be measured
- `hybernate_idle_seconds{namespace, workload}`: time since the workload's last activity
- `hybernate_workload_phase{namespace, workload, phase}`: 1 for each workload's current phase, and a `HybernateWorkloadStuck` alert for workloads left in `Pausing`, `Resuming`, or `Destroying` for 15 minutes

### Changed

- **Breaking:** idle detection is an activity clock. Each ManagedWorkload records its last activity in `status.activity`, and the idle action runs once there has been none for `idlePolicy.idleAfter` (default 1h). CPU above `idlePolicy.activity.cpuThreshold`% of requests, a pod template change, and the `hybernate.io/last-activity` / `hybernate.io/active-until` annotations count as activity; any one keeps the workload awake. There is no learning period: the forecast no longer gates idle detection, and only defers a pause when it is confident demand is coming
- **Breaking:** `idlePolicy.cpuIdleThreshold`, `memoryIdleThreshold`, `gracePeriod`, and `signals` are removed, along with the `auto` idle action (use `pause`). The WorkloadPolicy default idle policy is now `{action: pause, idleAfter: 1h, autoResume: true}`
- A paused workload wakes when an activity annotation newer than the pause, or a future `active-until`, is set on the ManagedWorkload or its target
- Drift detection only applies to paused workloads. Hybernate no longer owns the replica count of a running workload, so changes by its team or an autoscaler are not treated as drift; `conflictAction` now governs what happens when a paused workload is scaled up externally
- Events are emitted through the `events.k8s.io/v1` API (`events.EventRecorder`) instead of the deprecated `record.EventRecorder`. Each event now carries an action (e.g. `Pause`, `EvaluateIdle`, `CheckDrift`). The operator's ClusterRole gains `create`/`patch` on `events.k8s.io` events; the Helm chart and kustomize RBAC include it

### Removed

- `hybernate_idle_signal_result` and `hybernate_idle_fluke_total`, which described the grace-period model
- **Breaking:** prediction-driven replica scaling. `spec.scalePolicy` (min/max replicas, `overrideReplicas`, stabilization, step limits, and scale-down guards), `status.scale`, the `Scaling` phase, the `WorkloadPolicy` default `scalePolicy`, and the `hybernate_scale_events_total`, `hybernate_scale_replicas`, and `hybernate_scale_guard_blocked_total` metrics are gone. Sizing a running workload is left to HPA or KEDA; Hybernate pauses and resumes. Existing `scalePolicy` fields are dropped the next time a ManagedWorkload is written
- **Breaking:** the cluster-scoped `HybernateReport` CRD and its controller. Delete the CRD after upgrading (`kubectl delete crd hybernatereports.hybernate.io`). Per-workload cost stays in each ManagedWorkload's status
- **Breaking:** the cluster-wide gauges the report controller published: `hybernate_workloads_total`, `hybernate_active_workloads`, `hybernate_paused_workloads`, `hybernate_destroyed_workloads`, `hybernate_cost_*`, and `hybernate_resource_reduction_*`. Operator metrics now cover health; cost is shown per workload
- **Breaking:** the `Wasteful` classification and right-sizing estimates. `cpuWastefulThreshold`, `memoryWastefulThreshold`, `rightSizeTarget`, and `status.summary.wasteful` are removed from WorkloadPolicy. Discovery classifies workloads as Active or Idle, projected savings count only what pausing idle workloads would save, and auto-manage and `kubectl hybernate export` no longer pick up over-provisioned workloads that would never go idle
- **Breaking:** the Grafana dashboard (Helm value `grafana.enabled`, `config/grafana/`) and the never-populated `hybernate_discovery_estimated_savings_dollars` gauge
- Alert rules for low prediction confidence, regime changes, frequent drift, slow discovery scans, dry-run activity, and empty discovery. The shipped rules are now reconcile errors, operator down, stuck workloads, missing targets, and PVC retention expiry

### Fixed

- A resume completes when the workload's replicas are Ready, not when its pods exist. The check read the scale subresource, which counts pods that are still starting, such as a database replaying its log
- Cost is tracked for running workloads. Cost accumulation ran at the end of a reconcile that the automation step always returned from first, so `status.cost` was only ever updated for paused and destroyed workloads
- ManagedWorkload status is no longer written on every check. Values that change continuously (cost totals and the activity clock's timestamps) are written at most every 5 minutes, and phase, condition, and lifecycle changes are written immediately; activity seen between writes is held in memory
- `HybernatePVCRetentionExpiring` no longer fires permanently once PVCs are cleaned up or retention is cancelled; the gauge is set to 0 in both cases, which matched `< 86400`
- Resuming a paused workload restores its previous replica count. Since 0.1.7 the controller created the pause status early to hold the resource snapshot, which made the pauser skip recording the replica count, so every resume came back with a single replica

- Two ManagedWorkloads targeting the same workload no longer block each other. The duplicate check OR'd the UID tie-breaker in unconditionally, so when the older CR had the larger UID both were marked `DuplicateTarget` and neither managed the target. The oldest CR now always wins, with UID breaking ties only for CRs created in the same second
- A blocked duplicate now takes over when the owning ManagedWorkload is deleted. Previously it was never reconciled again, so it stayed blocked until something else touched it
- The `DuplicateTarget` warning event fires once when the conflict is detected, not on every recheck
- Workloads no longer get stuck in `Pausing`, `Resuming`, or `Destroying` after a transient failure. These intermediate phases were persisted before the action ran, and nothing acted on them afterwards, so one failed API call stranded the workload for good. Interrupted transitions are now retried until they finish
- Destroy is idempotent: a target that is already gone counts as destroyed, so a delete that succeeded before its status update failed no longer errors on every retry
- The resource snapshot used for savings is persisted before the delete is attempted, so it survives a failed attempt instead of being re-captured from a target that may no longer exist
- Discovery ranks workloads by their numeric savings. It compared the formatted dollar strings, so `$9.42` ranked above `$11.68`, and in namespaces over 500 workloads the cap kept the wrong ones
- The discovery summary counts every scanned workload, matching the cost and savings totals, instead of only the first 500
- Savings for paused and destroyed workloads price memory on the per-replica request, like CPU. The snapshot took CPU from requests but memory from live usage, which skewed savings and disagreed with discovery estimates
- Taking a resource snapshot no longer panics with an integer divide-by-zero when the target has zero replicas
- Taking a resource snapshot no longer emits a `TargetNotFound` warning or rewrites the target condition as a side effect
- Workloads no longer sit in `Observing` indefinitely with no explanation when CPU metrics can't be read (#10). A new `MetricsAvailable` condition reports `NoPodMetrics` or `MetricsUnavailable` with the cause, and a warning event fires when it first fails
- A target scaled to zero replicas feeds the forecast an observation of zero demand instead of being treated as missing data
- Status conditions now update their reason and message when the cause changes, even if the status stays the same
- A change to a Deployment only re-queues ManagedWorkloads that target that Deployment, and likewise for StatefulSets. The target watch matched on name alone, so a Deployment and a StatefulSet with the same name re-queued each other's ManagedWorkloads

## [0.1.7] - 2026-04-08

### Fixed

- Reconciler now initializes the idle detector and scaler in `initDefaults`; without these, the first reconcile of any workload with an `IdlePolicy` or `ScalePolicy` panicked with a nil-pointer dereference in production (#10)
- Cost status is now persisted after `accumulateCost`; previously the function mutated `Status.Cost` in memory but no caller called `Status().Update`, so cost data was silently dropped every reconcile
- First-pause resource snapshot is no longer lost; `Status.Pause` is now initialized before the snapshot is assigned, restoring the baseline used for savings calculations
- `resolveIdleAction` defensively nil-checks `IdlePolicy` to prevent the same class of panic from resurfacing via future callers
- Monthly cost reset now compares year and month so cross-year boundaries or backward clock jumps roll the bucket over instead of appending to the prior month

## [0.1.6] - 2026-03-23

### Changed

- Idle thresholds are now percentages of resource request instead of absolute values (cpuIdleThreshold default: 10%, memoryIdleThreshold default: 10%), making idle detection scale-fair across workloads of all sizes
- Added `Replicas()` and `MemoryRequestPerReplica()` to metrics reader for replica-aware threshold computation
- Signal checker thresholds now account for replica count to match total usage comparison
- Prediction cross-checks convert predicted demand to percentage of total request
- kubectl plugin installation docs now show curl download first, Krew second

### Fixed

- Prediction safety net in idle detection no longer silently bypassed on metrics errors
- Auto-resume percentage calculation now correctly uses total request (per-replica x replicas) from paused snapshot

## [0.1.5] - 2026-03-22

### Fixed

- Wasteful classification now requires both CPU and memory to be below thresholds (AND instead of OR), preventing memory-heavy workloads from being misclassified
- Renamed `utilizationPercent` to `cpuUtilizationPercent` for consistency with `memoryUtilizationPercent`
- Updated config/samples to match current API

## [0.1.4] - 2026-03-22

### Fixed

- Helm chart now pushes to separate GHCR path (`ghcr.io/okedeji/charts/hybernate`) to avoid collision with Docker image package
- Fixed Helm install URLs across README and docs

### Added

- OSS community files: LICENSE (Apache 2.0), CODE_OF_CONDUCT.md, SECURITY.md, CONTRIBUTING.md
- README rewrite with improved pitch, corrected Quick Start YAML, and architecture overview

## [0.1.2] - 2026-03-21

### Added

- Memory-aware idle detection: workloads must have both CPU and memory below thresholds to be classified as idle
- `memoryIdleThreshold` field on ManagedWorkload and WorkloadPolicy CRDs (default 100Mi)
- `memoryWastefulThreshold` field on WorkloadPolicy CRD (default 30%)
- `ResourceReduction` in CostStatus tracking concrete CPU, memory, and replicas freed by Hybernate actions
- `TotalResourceReduction` in HybernateReport for cluster-wide resource reduction aggregation
- Prometheus metrics: `hybernate_resource_reduction_cpu_millicores`, `hybernate_resource_reduction_memory_bytes`

### Changed

- Renamed `idleThreshold` to `cpuIdleThreshold` and `wastefulThreshold` to `cpuWastefulThreshold` for clarity
- Renamed `monthlySavings` to `estimatedMonthlySavings` to reflect that savings depend on cluster autoscaler node removal
- Renamed `costWithoutManagement` to `estimatedCostWithoutManagement`
- Renamed `totalMonthlySavings` to `estimatedTotalSavings` in HybernateReport
- Renamed `estimatedSavings` to `estimatedPotentialSavings` in WorkloadPolicy discovery
- Prometheus metric `hybernate_cost_savings_dollars` renamed to `hybernate_cost_estimated_savings_dollars`
- Prometheus metric `hybernate_cost_without_management_dollars` renamed to `hybernate_cost_estimated_without_management_dollars`
- Wasteful classification now considers memory utilization (CPU OR memory below threshold)

## [0.1.1] - 2026-03-19

### Added

- MkDocs documentation site with full user guides and API reference
- Cluster autoscaler configuration guide (Cluster Autoscaler, Karpenter, autopilot modes)
- LaTeX formulas for Holt-Winters forecasting model in docs
- Helm values reference page
- Scaling concepts page

### Changed

- Cost tracking is now always enabled (removed `enabled` field from CostTrackingSpec)
- PVC retention can be cancelled by removing `pvcRetention` from spec
- Simplified quick example on landing page

### Fixed

- Idle detection docs corrected to reflect forecast confirmation gate and auto-resume behavior
- Scaling pipeline docs corrected to remove incorrect signal consensus step

## [0.1.0] - 2026-03-17

### Added

- ManagedWorkload CRD (v1alpha1) for per-workload lifecycle management
- WorkloadPolicy CRD for namespace-wide discovery, classification, and auto-manage
- HybernateReport CRD for cluster-wide cost aggregation
- Idle detection with signal consensus, forecast confirmation gate, and grace period
- CPU threshold and Prometheus PromQL signal providers
- Holt-Winters double seasonal forecasting with confidence scoring and anomaly detection
- Forecast-driven scaling with stabilization, clamping, step limits, and guard probes
- Pause, resume, and destroy lifecycle actions
- Pause expiry with configurable resume or destroy action
- PVC retention with scheduled cleanup and warning events
- Manual replica override via desiredReplicas
- External replica drift detection and reconciliation
- Dry-run mode for observing operator decisions without taking action
- Auto-resume driven by forecast engine
- Cost tracking with per-workload and cluster-wide savings calculation
- Custom cost rates for CPU, memory, and storage
- Prometheus metrics, alerting rules, and Grafana dashboard
- Helm chart with ServiceMonitor, PrometheusRule, and NetworkPolicy support
- kubectl hybernate export plugin for generating ManagedWorkload manifests
- Krew plugin manifest for kubectl plugin distribution
- Multi-arch Docker images (linux/amd64, linux/arm64)
- Cosign image signing and SBOM generation
- Release workflow with cross-platform builds and Helm chart publishing

[Unreleased]: https://github.com/okedeji/hybernate/compare/v0.1.7...HEAD
[0.1.7]: https://github.com/okedeji/hybernate/compare/v0.1.6...v0.1.7
[0.1.6]: https://github.com/okedeji/hybernate/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/okedeji/hybernate/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/okedeji/hybernate/compare/v0.1.2...v0.1.4
[0.1.2]: https://github.com/okedeji/hybernate/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/okedeji/hybernate/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/okedeji/hybernate/releases/tag/v0.1.0
