# Changelog

All notable changes to Hybernate will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.0] - Unreleased

The relaunch. The 0.1.x releases were early experiments, and 0.2.0 is a fresh start: install it new rather than upgrading.

### Security

- The doorman's exposure is documented, with an example NetworkPolicy: anyone who can reach a doorman pod can reach every routed, paused workload on its Service ports, and the workload's own NetworkPolicy sees the doorman as the source. See [Security](https://okedeji.io/hybernate/reference/security/)
- The doorman only dials Pod-backed IP endpoints of EndpointSlices the EndpointSlice controller manages, and never loopback, link-local, or cloud metadata addresses (AWS's `fd00:ec2::254`, Alibaba Cloud's `100.100.100.200`); it caps held connections (2048 per pod, 1024 per source IP, 256 per source IP and workload) and rate-limits wakes per source and overall, holding a caller over the wake limit, or showing it the waking-up page, while the wake is retried. The Helm value `doorman.networkPolicy.egress` (off by default) adds an egress NetworkPolicy allowing the doorman only the pod CIDRs and the API server
- The operator can only `get` ConfigMaps, one at a time, and leader election only needs Leases and Events: the chart no longer grants ConfigMap writes or unused leader-election verbs
- The scan's HTML report is written readable only by you, and every credential in a reported address is hidden: everything before the last `@`, and the values of password-, token- and key-like keys

- Released Helm charts are signed with cosign, like the image, and every release publishes a signed `checksums.txt` (`checksums.txt.sigstore.json`) covering the plugin binaries, `install.yaml`, the chart, and the SBOM, and GitHub build provenance for the image and those files (`gh attestation verify`). See [Security](https://okedeji.io/hybernate/reference/security/) for the commands
- Every pull request and release scans the Go code with govulncheck and the image with Trivy, failing on reachable Go vulnerabilities and on critical and high image vulnerabilities with a fix; each release attaches Trivy's report as `vulnerabilities.txt`. A release is scanned before anything is pushed
- Builds use Go 1.26.8, fixing 27 reachable vulnerabilities in the standard library, gRPC, OpenTelemetry, `golang.org/x/net`, and `golang.org/x/text`; CI built with Go 1.25.3. The base images are pinned by digest, every GitHub Action by commit, and Dependabot updates them weekly. Workflows get read-only permissions unless a job needs more

### Added

- `IdleVetoed` condition, reason `ForecastExpectsDemand`, while a confident forecast holds off a pause; its message names the hour the forecast expects demand, and an `IdleVetoed` event marks the start of each veto
- `status.cost` is per calendar month and priced on requests (see Changed): `tracked`, `lastAccumulatedAt`, `awakeCPUHours`, `awakeMemoryHours`, `storageHours`, `pausedCPUHours`, `pausedMemoryHours`, `costThisMonth`, `savedThisMonth`, `costWithoutHybernateThisMonth`, `projectedMonthlyCost`, `projectedMonthlySavings`, `running`, `pricedAt`, `pricedTemplateHash`. `kubectl get managedworkloads` gains a `Saved` column. `status.dryRun` gains `freedCPUHours` and `freedMemoryHours`
- `status.prediction.state`: the forecast's learned state, kept in the ManagedWorkload. State that can't be read is discarded with a `ForecastReset` warning event
- Operator flags `--max-concurrent-reconciles` (4) and `--timezone` (UTC; the forecast's hours and weekdays follow it through daylight saving)
- Helm values `crds.install`, `maxConcurrentReconciles`, `timezone`, `logEncoder`, `metrics.readerSubjects` (bound to a new `<fullname>-metrics-reader` ClusterRole, so Prometheus can scrape secure metrics), `doorman.podDisruptionBudget`, `doorman.nodeSelector`, `doorman.tolerations`, `doorman.affinity`, `doorman.topologySpreadConstraints`, `nameOverride` and `fullnameOverride`; and a `values.schema.json` that rejects invalid values at install
- A doorman ServiceMonitor, a `HybernateDoormanDown` alert, and `kubectl apply -k config/prometheus` for kustomize installs
- `hybernate_doorman_proxied_connections` and `hybernate_doorman_port_conflicts`; `hybernate_doorman_wakes_total` gains the results `canceled`, `ignored`, `limited` and `refused`
- Wake on request: a client that stays connected without sending anything for 3 seconds wakes the workload, for protocols where the server speaks first, such as MySQL; a connection closed without sending anything doesn't. HTTP `GET` and `HEAD` requests from probes and scrapers (`kube-probe/`, `Prometheus/`, `vm_promscrape`, `GrafanaAgent/`, `Alloy/`, `OpenTelemetry Collector`, `otelcol`, `Datadog Agent/`, `ELB-HealthChecker/`, `GoogleHC/`, `Envoy/HC`) get a `503` and don't wake it, until a pod is Ready, when they're passed through; other methods from them, such as Prometheus remote write, wake it. A connection to a workload that already has a Ready pod is passed straight through without waiting for the caller to speak, and a held caller that sends a body and leaves is let go at once. HTTPS health checks on the traffic port can't be recognised and wake the workload; see the compatibility page. `hybernate.io/doorman-ignore-ports` on a Service keeps ports off the doorman. IPv6 and dual-stack Services are routed, one EndpointSlice per family. New `WakeOnRequest` reasons: `ServedByOtherPods`, `UnsupportedIPFamily`, `RoutingFailed`, `DoormanDisabled`
- `kubectl hybernate`: kubectl's connection flags on every command (`--kubeconfig`, `--context`, `--cluster`, `--user`, `--as`, `--token`, `--server`, `--request-timeout`, and the rest); `-A`/`--all-namespaces` on `status` and `scan`; a `version` command; `wake` and `deps` take the workload's name, `kind/name`, or the ManagedWorkload's name, and list the candidates when a name is ambiguous; `--timeout` on `status`, `deps` and `enable`
- `kubectl hybernate scan` reaches any Prometheus: `--prometheus-selector` reads one cluster's series from a store that holds many, `--prometheus-header` (such as `X-Scope-OrgID` for Mimir), `--prometheus-bearer-token-file`, `--prometheus-ca-file` and `--prometheus-insecure-skip-verify`. A `--prometheus-url` that doesn't answer like a Prometheus API fails the scan
- `kubectl hybernate scan` exits 1, after writing its report, when it couldn't read something your access allows; JSON and YAML gain `namespaces` and `incomplete`, and an empty cluster gives `"workloads": []`
- Krew installs the plugin on Linux and macOS (amd64 and arm64) and Windows (amd64)
- `make test` validates every ManagedWorkload in the docs and `config/samples` against the CRD
- Protected namespaces: label a namespace `hybernate.io/protected: "true"`, or match the Helm value `protectedNamespaces` (`--protected-namespaces`, patterns such as `prod-*`), and Hybernate never manages anything in it: nothing is opted in, a ManagedWorkload there reports `Protected=True` and is never paused, and a workload it had paused is woken. `hybernate.io/allow-protected: "true"` on the namespace allows it. The scan shows such workloads as `(protected)` and leaves them out of what pausing could save
- `watchNamespaces` (`--watch-namespaces`): Hybernate works only in those namespaces, with a Role and RoleBinding in each for the operator and the doorman instead of ClusterRoles, so Kubernetes refuses it anything elsewhere. The operator keeps one ClusterRole, to read Namespaces and Nodes
- [Data and Access](https://okedeji.io/hybernate/reference/data-and-access/): everything the operator, doorman, and plugin read, write, and send
- Learned dependencies: Hybernate reads the environment of every workload it manages, as the scan does, and holds and wakes the workloads its addresses name like a `dependsOn`, headless addresses included, with no YAML to write. They're kept in `status.learnedDependencies`, with a `DependenciesLearned` event when they change, and learned again when the pod template changes and hourly. They're also learned from wakes: the doorman records where the request that woke a workload came from (`hybernate.io/last-request-from`), and when the pod that sent it belongs to a workload Hybernate manages, that workload depends on the one it woke (`source: wake`). `hybernate.io/ignore-dependencies` drops one. The scan shows them as connected by Hybernate, and `kubectl hybernate deps NAME` shows a workload's dependencies and dependents, in any namespace, with where each link came from. See [Learned dependencies](https://okedeji.io/hybernate/concepts/dependencies/#learned-dependencies)
- HPA and KEDA: the `Autoscaled` condition says what scales a workload and its range. A workload a KEDA ScaledObject scales is paused through KEDA, with `autoscaling.keda.sh/paused-replicas: "0"` on the ScaledObject (`status.pause.scaledObject`), so KEDA doesn't scale it back up; a wake holds it at the restored replicas until they're Ready, then gives back the ScaledObject's own `paused-replicas` value, if it had one. A workload resumes within its HPA's or ScaledObject's range, and to at least one replica. The operator's ClusterRole gains read on HPAs, and read and patch on ScaledObjects. See [HPA and KEDA](https://okedeji.io/hybernate/guides/autoscalers/)
- Argo CD and Flux: when one sets a paused workload's replicas from Git, undoing the pause, the workload reports `GitOpsConflict=True` and a warning event with the one-time setting that leaves replicas to Hybernate, and Hybernate waits an hour before pausing again instead of fighting Git; the condition clears once a pause holds. `kubectl hybernate scan` lists workloads whose replicas are set from Git before they're opted in (JSON `replicasFromGit`). See [Argo CD and Flux](https://okedeji.io/hybernate/guides/gitops/)
- `kubectl hybernate scan`: a read-only report on one cluster's Deployments and StatefulSets, run with your own kubeconfig and nothing installed in the cluster. `--context` picks the cluster (EKS and GKE context names are shortened to cluster and region), `-n` the namespaces
    - **State:** each workload is idle, active, or paused right now, with the evidence (CPU against its request, when it was last deployed, activity annotations), and says how Hybernate is involved: unmanaged, dry-run, or live. Managed workloads are judged by Hybernate's activity clock and their own settings, others by `--cpu-threshold` (10) and `--idle-after` (1h). A workload someone scaled to zero shows as paused (unmanaged), counted apart from what Hybernate has paused
    - **Money, by source:** what each workload costs while running; what Hybernate has saved this month pausing live ones, from what it records; and what pausing could save a month, measured by Hybernate for dry-run workloads and estimated for unmanaged ones by replaying the activity clock over Prometheus history, with how long they'd have slept and how many times they'd have been woken. Prometheus is found by the Services the Prometheus Operator, kube-prometheus-stack, and the community chart create, and read through the API server's service proxy, or from `--prometheus-url`; `--window` sets how much history (7d; 0 for CPU right now only). Denied the service proxy, the scan prints the Role and RoleBinding an admin can create for that one Service. `--cpu-price` and `--memory-price` set your prices, and the report says which were assumed
    - **Dependencies:** what each workload connects to, from its environment variables, literal and from ConfigMaps, matched to the cluster's Services in any form cluster DNS gives them and followed to the workload behind them, with whether each is declared. Secrets are never read and passwords never shown
    - **Report page:** run in a terminal on a machine with a desktop, the scan also opens the report as a self-contained web page: what Hybernate saved and what pausing could save, four facts, workloads by namespace, every workload (sortable, filterable), the dependencies (without their addresses), and what the numbers mean. It prints cleanly to PDF. `--html FILE` saves it, also with `-o json`; `--open=false` keeps it closed; piped output, CI, and SSH sessions get none unless asked
    - **Output:** a terminal table, or `-o json|yaml` with the cluster's report at the top level. The notes say what the scan couldn't see
- Label opt-in: `hybernate.io/managed: "true"` on a Deployment or StatefulSet makes Hybernate manage it, and on a Namespace, every Deployment and StatefulSet in it; `hybernate.io/ignore: "true"` leaves one out. Hybernate creates a ManagedWorkload named after the workload, owned by it so it's deleted with it, marked `hybernate.io/from-label: "true"`, and without the workload's labels, so a GitOps tool's tracking label can't make the tool claim it. Settings are annotations on the workload or its namespace: `hybernate.io/dry-run`, `idle-after`, `cpu-threshold`, `auto-resume`, `depends-on`, `wake-on-request`, `wake-max-wait`, and `wake-page`. The workload's own wins, then its namespace's, then the cluster-wide defaults. Booleans are exactly `"true"` or `"false"`. A value that can't be read is reported in an `InvalidSetting` event and skipped, so the setting comes from the namespace, then the cluster default, then the built-in default; an unreadable `dry-run` turns dry-run on; a label value other than `"true"` is pointed out in a `LabelIgnored` event. Removing the label deletes the ManagedWorkload, and a ManagedWorkload someone wrote for the same workload always wins. The operator's ClusterRole gains read access to Namespaces
- Cluster-wide defaults for opted-in workloads: Helm values `defaults.idleAfter` (1h), `defaults.cpuThreshold` (10), and `defaults.dryRun` (false), passed as `--default-idle-after`, `--default-cpu-threshold`, and `--default-dry-run`
- Dry-run summary: a ManagedWorkload in dry-run keeps `status.dryRun` with when measuring started, how many times it would have been paused, how long it would have slept, and what that would have freed at its cost rates. A would-be pause runs from the clock running out until the next activity, when a paused workload would have been woken; its end is announced in the `ActivityResumed` event with the running total. The summary is dropped when dry-run ends. `kubectl hybernate scan` shows it for each workload in dry-run, priced at the scan's prices, with a total in the headline, and its next steps give the `kubectl hybernate enable` command
- `kubectl hybernate enable [KIND/]NAME`: ends dry-run for an opted-in workload by setting its own `hybernate.io/dry-run` annotation to `"false"`, which wins over its namespace's and the cluster default, or `spec.dryRun: false` on a ManagedWorkload written by hand. `--all` sets the namespace's annotation to `"false"` and drops the workloads' own. When Argo CD or Flux applies the workload it changes nothing, names the tool, prints the change to make in Git, and exits non-zero; `--force` changes the cluster anyway
- Waking-up page: a browser opening a paused workload is answered at once with a page that shows the address it opened and how long the workload has been starting, and nothing about the workload's names, and reloads every 3 seconds; the first reload after any of the workload's pods is Ready reaches the app. Only page loads get it (`GET` or `HEAD` with `Sec-Fetch-Mode: navigate`, or asking for HTML from older browsers); scripts, API clients, WebSockets, and other protocols are held as before, with the bytes read to tell them apart passed on unchanged. It's a `503` with `Cache-Control: no-store`. Opt out per workload with `wake.page: false`. `hybernate_doorman_wakes_total` gains the result `page`. The doorman also keeps a port open for 30 seconds after the workload is awake, passing connections straight through, so a reload that reaches it while proxies catch up lands on the app instead of an error
- `kubectl hybernate wake NAME`: marks a ManagedWorkload active now, which wakes it if paused (and the workloads it depends on), and waits until it's Running. `--for 2h` also keeps it awake for that long; `--wait=false` returns straight away. It refuses with the reason when activity can't wake the workload: a missing or ignored target, or another ManagedWorkload managing it
- `kubectl hybernate pause NAME`: pauses a workload now, rather than when its idle clock runs out, such as a preview environment at the end of the day. It's an ordinary pause: the workload wakes on a request through the doorman, on activity, by `autoResume`, or with `kubectl hybernate wake`. It waits until the workload is Paused, or Hybernate says why it won't be (`--wait=false` returns straight away), and warns when Argo CD or Flux last set the replicas and will likely undo the pause, with the one-time fix. It takes NAME as `wake` does. Underneath, it sets the `hybernate.io/pause-requested` annotation to a fresh token, which any tool can set: an annotation, so Argo CD and Flux leave it alone on a ManagedWorkload kept in Git. Hybernate acts on each token once, recording it in `status.lastPauseRequest`, and pauses by the rules of an idle pause: not while awake workloads depend on it, an `active-until` hold applies, its namespace is protected, or it's already at zero; under dry-run it counts a would-be pause. Asking overrides the hour Hybernate waits after a GitOps conflict. When a confident forecast expects demand within the hour, Hybernate declines the pause and says when, and `pause` asks whether to pause it anyway; `--yes`, or the `hybernate.io/pause-overrides-forecast: "true"` annotation with the request, pauses it without asking. The `PauseRequest` condition and an event say what came of each request. See [Pause now](https://okedeji.io/hybernate/guides/pause/#pause-now)
- Wake on request: a request to a paused workload's Service wakes it. While a workload is paused, the operator adds an EndpointSlice to each Service that selects it, pointing at the doorman, a new Deployment (two replicas) that runs the operator image with `--doorman`. The doorman holds each TCP connection, wakes the workload through the `hybernate.io/last-request` annotation, and passes the connection to a Ready pod, or closes it after `wake.maxWait` (default 2m) with a `RequestNotServed` warning event. A workload woken this way records `lastActivitySource: request`. On by default for workloads Hybernate pauses itself; opt out with `wake.onRequest: false`, or disable it cluster-wide with the Helm value `doorman.enabled: false`. Services behind GKE container-native load balancing (`cloud.google.com/neg`) aren't routed, with an `UnsupportedLoadBalancer` warning; see the compatibility page for the ingress controllers, Gateway API implementations, and load balancers checked. The `WakeOnRequest` condition reports whether a workload is routed; `hybernate_doorman_wakes_total`, `hybernate_doorman_wait_seconds`, `hybernate_doorman_held_connections`, and the `HybernateDoormanWakesFailing` alert cover it. The operator's ClusterRole gains read access to Services and full access to EndpointSlices
- Workload dependencies: `spec.dependsOn` lists the workloads a ManagedWorkload needs, in its own or another namespace. A dependency isn't paused while anything that depends on it is awake (`HeldByDependents`), waking a workload wakes its dependencies, and `waitForReady: true` holds a resume until a dependency's pods are Ready (`WaitingForDependencies`). Cycles stop both workloads from pausing (`DependencyCycle`); a dependency that doesn't exist is reported (`DependencyNotFound`) but doesn't block
- Prometheus activity sources: `idlePolicy.activity.prometheus` queries count as activity when they return a value above zero, such as an ingress request rate. Configure the endpoint with `--prometheus-url` (Helm value `prometheus.url`); path-prefixed endpoints (Thanos, Mimir, reverse proxies) work as-is. If a query can't be evaluated, the `PrometheusAvailable` condition reports `EndpointNotConfigured` or `QueryFailed` and the workload isn't paused
- `MetricsAvailable` reason `NoCPURequests`: idle detection doesn't act on a workload whose CPU utilization can't be measured
- `hybernate_idle_seconds{namespace, workload}`: time since the workload's last activity
- `hybernate_workload_phase{namespace, workload, phase}`: 1 for each workload's current phase, and a `HybernateWorkloadStuck` alert for workloads left in `Pausing` or `Resuming` for 15 minutes

### Changed

- Hybernate only pauses what's running, and only starts what it stopped: a workload scaled to zero by someone else (a person, a pipeline, or KEDA) is left at zero. It isn't paused, woken by a request or activity, or routed to the doorman, and reports the new `ScaledToZero` condition. A pause recorded at zero replicas is handed back at zero, and hand-back events say how many replicas the workload was left at
- `status.cost` covers the current calendar month in UTC, and everything is priced on what the workload's pods request, sidecars included, not on what they use. Rates come from `costTracking.rates`, then the list price of the workload's nodes, then the defaults, and are applied when the figures are worked out, so a rate change reprices the month. The hours are kept to a billionth of an hour; time is settled at every phase change, and a gap of more than 2 hours counts as 2. Projections are `pending` until a day of the month has been tracked. Saved becomes money only when a cluster autoscaler removes the capacity a pause frees
- The forecast is an additive double-seasonal Holt-Winters model, stable at zero demand. Confidence is 1 − WAPE, daily over the last 24 observed hours and weekly over 168. A phase needs every hour of the day, or of the week, observed, and is lost 5 points below the threshold; a regime change demotes it one level from where it was before the anomalies began, and discards its evidence. A large spike that recurs in the same hour of the week is learned as a pattern rather than clipped as an outlier. Missed hours are skipped, not filled in. `prediction.confidence` must be at least 50
- `spec.target` can't be changed once set, since a paused target would be left at zero
- `idlePolicy.activity.cpuThreshold` is 1 to 100; 0 is rejected
- The chart installs the ManagedWorkload CRD as a template, upgrades it with each `helm upgrade`, and keeps it on `helm uninstall` (`crds.install: false` opts out)
- Logs are JSON by default (`logEncoder`, `--zap-encoder`). `image.tag` defaults to the chart's `appVersion`. Memory defaults rise to 128Mi requested and 512Mi limit for the operator, and 64Mi and 256Mi for the doorman, with Go's soft memory limit set to 90% of the container's (`GOMEMLIMIT`, if set, wins). The doorman caches only the EndpointSlices the EndpointSlice controller writes, cut down to what it reads, and its Service prefers dual-stack, so on a dual-stack cluster both families' Services are routed
- `metrics.secure: false` serves plain HTTP, and secure metrics can be scraped by subjects in `metrics.readerSubjects`. `HybernateDown` matches only the operator's scrape job, and `HybernateDoormanWakesFailing` counts only `timeout` and `error` results
- The doorman's pods inherit the operator's `nodeSelector`, `tolerations` and `affinity` unless `doorman.*` sets its own, spread across nodes and zones, and keep one replica up through drains. They wait 5 seconds before shutting down, and give held and proxied connections 25 seconds to finish, within a 40-second termination grace period
- Wake on request works with `watchNamespaces`: the operator always reads the doorman's own EndpointSlices, through a Role in the release namespace
- A Service is routed to the doorman only while none of its pods is Ready, so a Service shared with workloads that are running keeps serving from them. The operator reads the Service's own EndpointSlices from the API server, rather than caching every one in the cluster, and looks again every 30 seconds while other pods serve it
- The label opt-in finds a workload's ManagedWorkload by its target, names a new one `<name>-<kind>` when the workload's name is taken, and never retargets one. It keeps what no annotation sets: `prediction`, `costTracking`, the Prometheus activity queries, and `waitForReady` on a dependency the annotation still lists, so `kubectl patch` works on a label-created ManagedWorkload
- A paused workload wakes on any change to `hybernate.io/last-activity`, `last-request` or `active-until`, on the ManagedWorkload or its target, since the pause began, whatever time the value states, so a clock behind the operator's can't lose a wake. Paused workloads are looked at at least every 5 minutes, and 15 minutes before each hour
- Dry-run never reduces availability: a pause requested under dry-run is counted as a would-be pause instead, and a paused workload switched into dry-run is woken (`DryRunWake`)
- Prometheus activity queries count any series above zero; `NaN` and infinite values are ignored, so an all-`NaN` or empty result is no data and the other sources decide. Scalars are accepted, range vectors and responses over 4 MiB rejected
- `kubectl hybernate status` and `scan` read every namespace you can by default, and fall back to the context's namespace with a note when you can't list them all. `status` says in its NEXT column why a workload won't pause, shows a hand-written ManagedWorkload's name in brackets, and counts last month's savings as none
- The scan's headline share of what could be saved is measured against what the same workloads cost over the same time, for replayed history
- `kubectl hybernate scan` reads each namespace in a few paged calls, and says what it couldn't read. Workloads already scaled to zero by hand count toward nothing it says could be saved
- Kubernetes 1.30 and later is supported, and CI runs the end-to-end tests on 1.30 and 1.37. Kubernetes 1.26 to 1.29 are past their end of life upstream and with every cloud provider
- Costs and savings are priced at the on-demand list price of the nodes each workload runs on, from their instance type and region, for every AWS, Google Cloud, and Azure instance type, instead of AWS us-east-1 rates for everything. The rates are kept in `status.cost.listRates`, so a paused workload's savings stay priced where it ran. `costTracking.rates` still overrides them, and nodes without a list price use the previous defaults. The operator reads Nodes' metadata for this, so its ClusterRole gains `get`, `list`, and `watch` on nodes
- `kubectl hybernate scan` prices workloads the same way, and says which node types it priced at, which nodes had no list price, and how many workloads run on spot nodes, priced at on-demand. `--cpu-price` and `--memory-price` override node prices; JSON adds `nodePrices` and each workload's `onSpot`
- `autoResume` wakes a workload 15 minutes before an hour the forecast expects to be busy, instead of once that hour has started, so it's Ready when people arrive
- The forecast records each hour once it's over, at the peak CPU read in it, instead of one reading as the hour began, and keeps learning while a workload is paused behind the doorman: an hour spent paused is no demand, since a request would have woken it, and an hour a request woke it in is busy. It learns quiet hours, and reaches confidence sooner, from workloads that spend most of the day paused, and `autoResume` learns to wake a workload before the people who'd wake it arrive. An hour the operator didn't see from start to end, such as one it restarted in, is skipped. Awake workloads without an idle policy are checked every minute instead of hourly
- The forecast's veto holds back a pause for the hour under way as well as the next, so a pause isn't woken straight back by `autoResume`; why the veto couldn't judge the forecast is logged
- Idle detection is an activity clock. Each ManagedWorkload records its last activity in `status.activity`, and the idle action runs once there has been none for `idlePolicy.idleAfter` (default 1h). CPU above `idlePolicy.activity.cpuThreshold`% of requests, a pod template change, and the `hybernate.io/last-activity` / `hybernate.io/active-until` annotations count as activity; any one keeps the workload awake. There is no learning period: the forecast no longer gates idle detection, and only defers a pause when it is confident demand is coming
- A paused workload wakes when an activity annotation newer than the pause, or a future `active-until`, is set on the ManagedWorkload or its target
- A paused workload scaled up outside Hybernate wakes: whoever did it wants it running, so it goes back to Running and its idle clock starts again (`lastActivitySource: scaled-up`). `status.lastScaledUp` records when, how many replicas, and the field manager that did it. Hybernate doesn't manage the replica count of a running workload, so changes by its team or an autoscaler are left alone. `hybernate_external_scale_ups_total{by}` counts them
- Events are emitted through the `events.k8s.io/v1` API (`events.EventRecorder`) instead of the deprecated `record.EventRecorder`. Each event now carries an action (e.g. `Pause`, `EvaluateIdle`, `CheckReplicas`). The operator's ClusterRole gains `create`/`patch` on `events.k8s.io` events; the Helm chart and kustomize RBAC include it

### Fixed

- A pause is recorded before the workload is scaled, so a pause interrupted at any point, by a crash or a conflicting write, is finished or undone from the record, and a resume goes back to the exact replica count
- The `hybernate.io/ignore` label, a protected namespace, and deleting the ManagedWorkload all hand back a paused workload, scaled up and released from KEDA, and remove its doorman routing. A workload whose target is deleted stops routing to the doorman
- KEDA: a ScaledObject's own `paused-replicas` value is put back after a pause, and uninstalling KEDA, or deleting the ScaledObject, while a workload is paused no longer blocks its wake
- A forecast veto no longer holds back a pause already decided, and doorman routing problems never hold up a pause or a wake: they're retried with backoff from 5 seconds to 5 minutes
- A dependency in a namespace outside `watchNamespaces`, or that can't be read, is reported (`DependencyNotFound`, reason `DependencyNotVisible`) and never blocks; `waitForReady` doesn't wait for a dependency that can't start (unmanaged at zero replicas); learned dependencies never form a cycle that holds both workloads awake; and a dependency learned from a wake gets a valid status
- Activity is written to status promptly enough that a restart can't pause a workload early
- Events are emitted once, when something changes: forecast vetoes, routing warnings, learned dependencies, and the `DependentsAwake` warning, which fires once a manual pause begins
- Every series of a deleted workload is dropped from the metrics, and the operator reads only the target's own pods and claims, with deadlines on every call
- A GitOps tool that set the replicas in the same second as another field manager is reported as the one that woke a paused workload, so the `GitOpsConflict` is never missed
- The plugin formats negative dollars and durations correctly
- `kubectl hybernate scan` no longer hangs when the API server stops answering: each request gives up after 30 seconds, and the scan after `--timeout` (5m), with an error rather than a partial report
- Deleting the ManagedWorkload of a paused workload scales the workload back to the replicas it had first, instead of leaving it at zero
- Workloads with an injected sidecar, such as an Istio or Linkerd proxy, can pause. CPU usage was counted for every container in the pod but measured against requests from the pod template, which doesn't include injected sidecars, so the proxy's background work alone could keep a workload above the CPU threshold. Usage and requests now both cover the template's containers and native sidecars; `kubectl hybernate scan` measures workloads the same way
- Cost and savings include injected sidecars. Savings were priced on the pod template's requests, which leave out a sidecar injected at pod creation, often as large as the app itself; they're now priced on a running pod's requests, sidecars included. The operator's ClusterRole gains `get` and `list` on pods, which are read when a workload pauses and during discovery scans, not cached
- A resume completes when the workload's replicas are Ready, not when its pods exist. The check read the scale subresource, which counts pods that are still starting, such as a database replaying its log
- Cost is tracked for running workloads. Cost accumulation ran at the end of a reconcile that the automation step always returned from first, so `status.cost` was only ever updated for paused workloads
- ManagedWorkload status is no longer written on every check. Values that change continuously (cost totals and the activity clock's timestamps) are written at most every 5 minutes, and phase, condition, and lifecycle changes are written immediately; activity seen between writes is held in memory
- Resuming a paused workload restores its previous replica count. Since 0.1.7 the controller created the pause status early to hold the resource snapshot, which made the pauser skip recording the replica count, so every resume came back with a single replica

- Two ManagedWorkloads targeting the same workload no longer block each other. The duplicate check OR'd the UID tie-breaker in unconditionally, so when the older CR had the larger UID both were marked `DuplicateTarget` and neither managed the target. The oldest CR now always wins, with UID breaking ties only for CRs created in the same second
- A blocked duplicate now takes over when the owning ManagedWorkload is deleted. Previously it was never reconciled again, so it stayed blocked until something else touched it
- The `DuplicateTarget` warning event fires once when the conflict is detected, not on every recheck
- Workloads no longer get stuck in `Pausing` or `Resuming` after a transient failure. These intermediate phases were persisted before the action ran, and nothing acted on them afterwards, so one failed API call stranded the workload for good. Interrupted transitions are now retried until they finish
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

[Unreleased]: https://github.com/okedeji/hybernate/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/okedeji/hybernate/compare/v0.1.7...v0.2.0
[0.1.7]: https://github.com/okedeji/hybernate/compare/v0.1.6...v0.1.7
[0.1.6]: https://github.com/okedeji/hybernate/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/okedeji/hybernate/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/okedeji/hybernate/compare/v0.1.2...v0.1.4
[0.1.2]: https://github.com/okedeji/hybernate/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/okedeji/hybernate/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/okedeji/hybernate/releases/tag/v0.1.0
