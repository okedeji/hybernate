# Changelog

All notable changes to Hybernate will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Argo CD and Flux: when one sets a paused workload's replicas from Git, undoing the pause, the workload reports `GitOpsConflict=True` and a warning event with the one-time setting that leaves replicas to Hybernate, and Hybernate waits an hour before pausing again instead of fighting Git; the condition clears once a pause holds. `kubectl hybernate scan` lists workloads whose replicas are set from Git before they're opted in (JSON `replicasFromGit`). See [Argo CD and Flux](https://okedeji.io/hybernate/guides/gitops/)
- `kubectl hybernate scan`: a read-only report on one cluster's Deployments and StatefulSets, run with your own kubeconfig and nothing installed in the cluster. `--context` picks the cluster (EKS and GKE context names are shortened to cluster and region), `-n` the namespaces
    - **State:** each workload is idle, active, or paused right now, with the evidence (CPU against its request, when it was last deployed, activity annotations), and says how Hybernate is involved: unmanaged, dry-run, or live. Managed workloads are judged by Hybernate's activity clock and their own settings, others by `--cpu-threshold` (10) and `--idle-after` (1h). A workload someone scaled to zero shows as paused (unmanaged), counted apart from what Hybernate has paused
    - **Money, by source:** what each workload costs while running; what Hybernate has saved this month pausing live ones, from what it records; and what pausing could save a month, measured by Hybernate for dry-run workloads and estimated for unmanaged ones by replaying the activity clock over Prometheus history, with how long they'd have slept and how many times they'd have been woken. Prometheus is found by the Services the Prometheus Operator, kube-prometheus-stack, and the community chart create, and read through the API server's service proxy, or from `--prometheus-url`; `--window` sets how much history (7d; 0 for CPU right now only). Denied the service proxy, the scan prints the Role and RoleBinding an admin can create for that one Service. `--cpu-price` and `--memory-price` set your prices, and the report says which were assumed
    - **Dependencies:** what each workload connects to, from its environment variables, literal and from ConfigMaps, matched to the cluster's Services in any form cluster DNS gives them and followed to the workload behind them, with whether each is declared. Secrets are never read and passwords never shown
    - **Report page:** run in a terminal on a machine with a desktop, the scan also opens the report as a self-contained web page: what Hybernate saved and what pausing could save, four facts, workloads by namespace, every workload (sortable, filterable), the dependencies (without their addresses), and what the numbers mean. It prints cleanly to PDF. `--html FILE` saves it, also with `-o json`; `--open=false` keeps it closed; piped output, CI, and SSH sessions get none unless asked
    - **Output:** a terminal table, or `-o json|yaml` with the cluster's report at the top level. The notes say what the scan couldn't see
- Label opt-in: `hybernate.io/managed: "true"` on a Deployment or StatefulSet makes Hybernate manage it, and on a Namespace, every Deployment and StatefulSet in it; `hybernate.io/ignore: "true"` leaves one out. Hybernate creates a ManagedWorkload named after the workload, owned by it so it's deleted with it, marked `hybernate.io/from-label: "true"`, and without the workload's labels, so a GitOps tool's tracking label can't make the tool claim it. Settings are annotations on the workload or its namespace: `hybernate.io/dry-run`, `idle-after`, `cpu-threshold`, `auto-resume`, `depends-on`, `wake-on-request`, `wake-max-wait`, and `wake-page`. The workload's own wins, then its namespace's, then the cluster-wide defaults. A value that can't be read is reported in an `InvalidSetting` event and falls back to its default; a label value other than `"true"` is pointed out in a `LabelIgnored` event. Removing the label deletes the ManagedWorkload, and a ManagedWorkload someone wrote for the same workload always wins. The operator's ClusterRole gains read access to Namespaces
- Cluster-wide defaults for opted-in workloads: Helm values `defaults.idleAfter` (1h), `defaults.cpuThreshold` (10), and `defaults.dryRun` (false), passed as `--default-idle-after`, `--default-cpu-threshold`, and `--default-dry-run`
- Dry-run summary: a ManagedWorkload in dry-run keeps `status.dryRun` with when measuring started, how many times it would have been paused, how long it would have slept, and what that would have freed at its cost rates. A would-be pause runs from the clock running out until the next activity, when a paused workload would have been woken; its end is announced in the `ActivityResumed` event with the running total. The summary is dropped when dry-run ends. `kubectl hybernate scan` shows it for each workload in dry-run, priced at the scan's prices, with a total in the headline, and its next steps give the `kubectl hybernate enable` command
- `kubectl hybernate enable [KIND/]NAME`: ends dry-run for an opted-in workload by removing its `hybernate.io/dry-run` annotation, or setting it to `"false"` when the dry-run comes from the namespace. `--all` does the same for every workload in a namespace and the namespace itself. When Argo CD or Flux applies the workload it changes nothing, names the tool, prints the change to make in Git, and exits non-zero; `--force` changes the cluster anyway
- Waking-up page: a browser opening a paused workload is answered at once with a page that shows the address it opened, the wake's progress, and when the workload was paused, and reloads every 3 seconds until the workload is Ready, instead of a blank tab. Only page loads get it (`GET` or `HEAD` with `Sec-Fetch-Mode: navigate`, or asking for HTML from older browsers); scripts, API clients, WebSockets, and other protocols are held as before, with the bytes read to tell them apart passed on unchanged. It's a `503` with `Cache-Control: no-store`. Opt out per workload with `wake.page: false`. `hybernate_doorman_wakes_total` gains the result `page`. The doorman also keeps a port open for 30 seconds after the workload is awake, passing connections straight through, so a reload that reaches it while proxies catch up lands on the app instead of an error
- `kubectl hybernate wake NAME`: marks a ManagedWorkload active now, which wakes it if paused (and the workloads it depends on), and waits until it's Running. `--for 2h` also keeps it awake for that long; `--wait=false` returns straight away. It refuses with the reason when activity can't wake the workload: `desiredState: Paused`
- Wake on request: a request to a paused workload's Service wakes it. While a workload is paused, the operator adds an EndpointSlice to each Service that selects it, pointing at the doorman, a new Deployment (two replicas) that runs the operator image with `--doorman`. The doorman holds each TCP connection, wakes the workload through the `hybernate.io/last-request` annotation, and passes the connection to the first Ready pod, or closes it after `wake.maxWait` (default 2m) with a `RequestNotServed` warning event. A workload woken this way records `lastActivitySource: request`. On by default for workloads Hybernate pauses itself; opt out with `wake.onRequest: false`, or disable it cluster-wide with the Helm value `doorman.enabled: false`. Services behind GKE container-native load balancing (`cloud.google.com/neg`) aren't routed, with an `UnsupportedLoadBalancer` warning; see the compatibility page for the ingress controllers, Gateway API implementations, and load balancers checked. The `WakeOnRequest` condition reports whether a workload is routed; `hybernate_doorman_wakes_total`, `hybernate_doorman_wait_seconds`, `hybernate_doorman_held_connections`, and the `HybernateDoormanWakesFailing` alert cover it. The operator's ClusterRole gains read access to Services and full access to EndpointSlices
- Workload dependencies: `spec.dependsOn` lists the workloads a ManagedWorkload needs, in its own or another namespace. A dependency isn't paused while anything that depends on it is awake (`HeldByDependents`), waking a workload wakes its dependencies, and `waitForReady: true` holds a resume until a dependency's pods are Ready (`WaitingForDependencies`). Cycles stop both workloads from pausing (`DependencyCycle`); a dependency that doesn't exist is reported (`DependencyNotFound`) but doesn't block
- Prometheus activity sources: `idlePolicy.activity.prometheus` queries count as activity when they return a value above zero, such as an ingress request rate. Configure the endpoint with `--prometheus-url` (Helm value `prometheus.url`); path-prefixed endpoints (Thanos, Mimir, reverse proxies) work as-is. If a query can't be evaluated, the `PrometheusAvailable` condition reports `EndpointNotConfigured` or `QueryFailed` and the workload isn't paused
- `MetricsAvailable` reason `NoCPURequests`: idle detection doesn't act on a workload whose CPU utilization can't be measured
- `hybernate_idle_seconds{namespace, workload}`: time since the workload's last activity
- `hybernate_workload_phase{namespace, workload, phase}`: 1 for each workload's current phase, and a `HybernateWorkloadStuck` alert for workloads left in `Pausing` or `Resuming` for 15 minutes

### Changed

- Costs and savings are priced at the on-demand list price of the nodes each workload runs on, from their instance type and region, for every AWS, Google Cloud, and Azure instance type, instead of AWS us-east-1 rates for everything. The rates are kept in `status.cost.listRates`, so a paused workload's savings stay priced where it ran. `costTracking.rates` still overrides them, and nodes without a list price use the previous defaults. The operator reads Nodes' metadata for this, so its ClusterRole gains `get`, `list`, and `watch` on nodes
- `kubectl hybernate scan` prices workloads the same way, and says which node types it priced at, which nodes had no list price, and how many workloads run on spot nodes, priced at on-demand. `--cpu-price` and `--memory-price` override node prices; JSON adds `nodePrices` and each workload's `onSpot`
- `autoResume` wakes a workload 15 minutes before an hour the forecast expects to be busy, instead of once that hour has started, so it's Ready when people arrive
- The forecast keeps learning while a workload is paused behind the doorman: each paused hour is recorded as zero demand, since a request would have woken it. It learns quiet hours, and reaches confidence sooner, from workloads that spend most of the day paused
- **Breaking:** idle detection is an activity clock. Each ManagedWorkload records its last activity in `status.activity`, and the idle action runs once there has been none for `idlePolicy.idleAfter` (default 1h). CPU above `idlePolicy.activity.cpuThreshold`% of requests, a pod template change, and the `hybernate.io/last-activity` / `hybernate.io/active-until` annotations count as activity; any one keeps the workload awake. There is no learning period: the forecast no longer gates idle detection, and only defers a pause when it is confident demand is coming
- **Breaking:** `idlePolicy.cpuIdleThreshold`, `memoryIdleThreshold`, `gracePeriod`, and `signals` are removed, along with the `auto` idle action (use `pause`). The WorkloadPolicy default idle policy is now `{action: pause, idleAfter: 1h, autoResume: true}`
- A paused workload wakes when an activity annotation newer than the pause, or a future `active-until`, is set on the ManagedWorkload or its target
- **Breaking:** a paused workload scaled up outside Hybernate wakes: whoever did it wants it running, so it goes back to Running and its idle clock starts again (`lastActivitySource: scaled-up`). `status.lastScaledUp` records when, how many replicas, and the field manager that did it. Hybernate doesn't manage the replica count of a running workload, so changes by its team or an autoscaler are left alone. `hybernate_drift_detections_total{policy}` is now `hybernate_external_scale_ups_total{by}`
- Events are emitted through the `events.k8s.io/v1` API (`events.EventRecorder`) instead of the deprecated `record.EventRecorder`. Each event now carries an action (e.g. `Pause`, `EvaluateIdle`, `CheckReplicas`). The operator's ClusterRole gains `create`/`patch` on `events.k8s.io` events; the Helm chart and kustomize RBAC include it

### Removed

- **Breaking:** the destroy action. Hybernate never deletes a workload or its storage: it pauses, and a request wakes the workload. Removed: `idlePolicy.action` (pausing is the only action), `desiredState: Destroyed`, `spec.destroy` (`pvcRetention`, `pvcRetentionWarning`) and PVC cleanup, `spec.pause` (`expireAfter`, `expireAction`), the `Destroying` and `Destroyed` phases and `status.destroy`, the `hybernate_pause_expiry_actions_total` and `hybernate_pvc_retention_remaining_seconds` metrics, the `action` label on `hybernate_idle_detections_total`, and the `HybernatePVCRetentionExpiring` alert. The operator's ClusterRole no longer has `update` or `delete` on Deployments and StatefulSets, or `delete` on PVCs: it reads them and scales them through the scale subresource. Delete environments nobody comes back to where they were created, from Git, their pipeline, or a TTL tool such as kube-janitor. Remove these fields from ManagedWorkloads before upgrading; a ManagedWorkload left `Destroyed` should be deleted
- **Breaking:** `spec.conflictAction`. A paused workload scaled up outside Hybernate now always wakes; scaling it back to zero (`enforce`) fought the people and tools that scaled it, and `warn` left it running while reporting Paused. Existing values are dropped the next time a ManagedWorkload is written
- **Breaking:** the `WorkloadPolicy` CRD, its controller, and namespace discovery. Opt workloads or namespaces in with the `hybernate.io/managed` label instead, and use `kubectl hybernate scan` to see what's idle. Delete the CRD after upgrading (`kubectl delete crd workloadpolicies.hybernate.io`). ManagedWorkloads a WorkloadPolicy created are left in place; delete them, or label their workloads to have Hybernate take them over
- **Breaking:** `kubectl hybernate export`. The decision to manage a workload now lives in Git as a label on it, so there are no ManagedWorkload manifests to generate
- **Breaking:** the `hybernate.io/auto-discovered` label and `hybernate.io/workload-policy` annotation, and the `hybernate_discovery_scan_duration_seconds`, `hybernate_discovery_workloads`, and `hybernate_discovery_auto_managed_total` metrics
- `hybernate_idle_signal_result` and `hybernate_idle_fluke_total`, which described the grace-period model
- **Breaking:** prediction-driven replica scaling. `spec.scalePolicy` (min/max replicas, `overrideReplicas`, stabilization, step limits, and scale-down guards), `status.scale`, the `Scaling` phase, the `WorkloadPolicy` default `scalePolicy`, and the `hybernate_scale_events_total`, `hybernate_scale_replicas`, and `hybernate_scale_guard_blocked_total` metrics are gone. Sizing a running workload is left to HPA or KEDA; Hybernate pauses and resumes. Existing `scalePolicy` fields are dropped the next time a ManagedWorkload is written
- **Breaking:** the cluster-scoped `HybernateReport` CRD and its controller. Delete the CRD after upgrading (`kubectl delete crd hybernatereports.hybernate.io`). Per-workload cost stays in each ManagedWorkload's status
- **Breaking:** the cluster-wide gauges the report controller published: `hybernate_workloads_total`, `hybernate_active_workloads`, `hybernate_paused_workloads`, `hybernate_cost_*`, and `hybernate_resource_reduction_*`. Operator metrics now cover health; cost is shown per workload
- **Breaking:** the `Wasteful` classification and right-sizing estimates. `cpuWastefulThreshold`, `memoryWastefulThreshold`, `rightSizeTarget`, and `status.summary.wasteful` are removed from WorkloadPolicy. Discovery classifies workloads as Active or Idle, projected savings count only what pausing idle workloads would save, and auto-manage and `kubectl hybernate export` no longer pick up over-provisioned workloads that would never go idle
- **Breaking:** the Grafana dashboard (Helm value `grafana.enabled`, `config/grafana/`) and the never-populated `hybernate_discovery_estimated_savings_dollars` gauge
- Alert rules for low prediction confidence, regime changes, frequent drift, slow discovery scans, dry-run activity, and empty discovery. The shipped rules are now reconcile errors, operator down, stuck workloads, and missing targets

### Fixed

- `kubectl hybernate scan` no longer hangs when the API server stops answering: each request gives up after 30 seconds, and the scan after `--timeout` (5m), with an error rather than a partial report
- Deleting the ManagedWorkload of a paused workload scales the workload back to the replicas it had first, instead of leaving it at zero
- Workloads with an injected sidecar, such as an Istio or Linkerd proxy, can pause. CPU usage was counted for every container in the pod but measured against requests from the pod template, which doesn't include injected sidecars, so the proxy's background work alone could keep a workload above the CPU threshold. Usage and requests now both cover the template's containers and native sidecars; `kubectl hybernate scan` measures workloads the same way
- Cost and savings include injected sidecars. Savings were priced on the pod template's requests, which leave out a sidecar injected at pod creation, often as large as the app itself; they're now priced on a running pod's requests, and running cost counts every container's usage. The operator's ClusterRole gains `get` and `list` on pods, which are read when a workload pauses and during discovery scans, not cached
- A resume completes when the workload's replicas are Ready, not when its pods exist. The check read the scale subresource, which counts pods that are still starting, such as a database replaying its log
- Cost is tracked for running workloads. Cost accumulation ran at the end of a reconcile that the automation step always returned from first, so `status.cost` was only ever updated for paused workloads
- ManagedWorkload status is no longer written on every check. Values that change continuously (cost totals and the activity clock's timestamps) are written at most every 5 minutes, and phase, condition, and lifecycle changes are written immediately; activity seen between writes is held in memory
- Resuming a paused workload restores its previous replica count. Since 0.1.7 the controller created the pause status early to hold the resource snapshot, which made the pauser skip recording the replica count, so every resume came back with a single replica

- Two ManagedWorkloads targeting the same workload no longer block each other. The duplicate check OR'd the UID tie-breaker in unconditionally, so when the older CR had the larger UID both were marked `DuplicateTarget` and neither managed the target. The oldest CR now always wins, with UID breaking ties only for CRs created in the same second
- A blocked duplicate now takes over when the owning ManagedWorkload is deleted. Previously it was never reconciled again, so it stayed blocked until something else touched it
- The `DuplicateTarget` warning event fires once when the conflict is detected, not on every recheck
- Workloads no longer get stuck in `Pausing` or `Resuming` after a transient failure. These intermediate phases were persisted before the action ran, and nothing acted on them afterwards, so one failed API call stranded the workload for good. Interrupted transitions are now retried until they finish
- The resource snapshot used for savings is persisted before the delete is attempted, so it survives a failed attempt instead of being re-captured from a target that may no longer exist
- Discovery ranks workloads by their numeric savings. It compared the formatted dollar strings, so `$9.42` ranked above `$11.68`, and in namespaces over 500 workloads the cap kept the wrong ones
- The discovery summary counts every scanned workload, matching the cost and savings totals, instead of only the first 500
- Savings for paused workloads price memory on the per-replica request, like CPU. The snapshot took CPU from requests but memory from live usage, which skewed savings and disagreed with discovery estimates
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
