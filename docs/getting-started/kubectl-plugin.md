# kubectl Plugin

The `kubectl hybernate` plugin scans a cluster for idle workloads, shows what Hybernate is doing in it, ends dry-run for workloads you've opted in, and wakes paused ones.

`scan` and `status` read every namespace you can, and fall back to your kubeconfig context's namespace, with a note, when your access doesn't allow listing them all; `-n` names namespaces instead, and `-A` (`--all-namespaces`) insists on every one, failing rather than falling back. `wake`, `deps` and `enable` work in one namespace: `-n`, or your context's, as kubectl does.

Every command takes kubectl's connection flags: `--kubeconfig`, `--context`, `--cluster`, `--user`, `--as`, `--as-group`, `--as-uid`, `--token`, `--server`, `--certificate-authority`, `--client-certificate`, `--client-key`, `--insecure-skip-tls-verify`, `--tls-server-name`, `--proxy-url`, `--username`, `--password`, `--disable-compression` and `--request-timeout`. Each request to the API server gives up after 30 seconds unless `--request-timeout` sets otherwise, so a cluster that stops answering ends a command with an error, not a hang. `kubectl hybernate version` prints the plugin's version.

## Installation

=== "curl"

    Download the binary from the [releases page](https://github.com/okedeji/hybernate/releases) and place it in your `PATH`:

    ```bash
    # macOS (Apple Silicon)
    curl -LO https://github.com/okedeji/hybernate/releases/latest/download/kubectl-hybernate-darwin-arm64.tar.gz
    tar xzf kubectl-hybernate-darwin-arm64.tar.gz
    chmod +x kubectl-hybernate-darwin-arm64
    mv kubectl-hybernate-darwin-arm64 /usr/local/bin/kubectl-hybernate

    # Linux (amd64)
    curl -LO https://github.com/okedeji/hybernate/releases/latest/download/kubectl-hybernate-linux-amd64.tar.gz
    tar xzf kubectl-hybernate-linux-amd64.tar.gz
    chmod +x kubectl-hybernate-linux-amd64
    mv kubectl-hybernate-linux-amd64 /usr/local/bin/kubectl-hybernate
    ```

=== "Krew"

    ```bash
    kubectl krew install --manifest-url \
      https://github.com/okedeji/hybernate/releases/latest/download/krew-hybernate.yaml
    ```

    Krew installs the plugin on Linux and macOS (amd64 and arm64) and on Windows (amd64).

    !!! note
        This uses a custom manifest URL. Once the plugin is accepted into the [krew-index](https://github.com/kubernetes-sigs/krew-index), you'll be able to install with just `kubectl krew install hybernate`.

=== "Source"

    ```bash
    git clone https://github.com/okedeji/hybernate.git
    cd hybernate
    make build-plugin
    # Binary is at bin/kubectl-hybernate
    ```

## Scan a Cluster

```bash
kubectl hybernate scan
```

```
staging (EKS us-east-1): 214 workloads in 38 namespaces

  Hybernate has 12 workloads paused right now, freeing $1.84 an hour.
  Hybernate has saved $96 this month pausing 14 live workloads.
  3 workloads are in dry-run: measured by Hybernate since starting, they would have slept 312 hours, freeing $41,
  about $140 a month.
  4 workloads are scaled to zero by hand; Hybernate can pause them while idle and wake them on the
  next request instead.
  61 workloads are idle right now, reserving 48.0 vCPU and 190.0 GiB of memory.
  They cost $3,180/month while running, $4.36 an hour: each hour asleep frees that.
  Replaying the last 7 days, Hybernate would have paused 58 of 190 unmanaged workloads for 6,120 hours in all,
  and freed $1,240: about $5,380/month.

  NAMESPACE    WORKLOAD               STATE                BECAUSE                                                  COST/MO   SAVED THIS MONTH   COULD SLEEP   WAKES   COULD SAVE/MO
  preview-42   statefulset/postgres   idle (unmanaged)     CPU under 1% of its request; last deployed 23 days ago   $140      -                  167h          0       $139
  preview-7    deployment/api         idle (dry-run)       no activity for 5h; measuring since Oct 1                $96       -                  41h           4       $40
  preview-3    deployment/web         paused (live)        paused 3h ago                                            $48       $12                -             -       -
  preview-9    deployment/demo        paused (unmanaged)   scaled to zero, not by Hybernate                         $0        -                  -             -       -
  payments     deployment/ledger      active (unmanaged)   CPU 64% of its request                                   $210      -                  0h            0       $0
  ...

Notes:
  - the history replay sees CPU and rollouts only; requests and activity annotations aren't in Prometheus, so a workload used with little CPU can look like it would sleep more than it would
  - 2 workloads set no CPU requests, so their use can't be measured: kube-tools/agent, ops/exporter

Costs use on-demand list prices for its 4 node types, m6i.xlarge, m6i.2xlarge, r6i.xlarge and 1 more, in AWS us-east-1. 6 workloads run on spot nodes, priced at on-demand, so they cost less than shown.
Pass --cpu-price and --memory-price for yours.
```

`scan` only reads, with your own kubeconfig, and needs nothing installed in the cluster. It works before you install Hybernate, and after: once Hybernate manages a workload, the scan shows what it has paused and reads its activity clock.

**How it decides a workload is idle**, using the same signals Hybernate uses to pause:

- **Workloads Hybernate manages:** the activity clock it keeps for them, which already combines every source configured, including Prometheus queries. Idle means no activity for `idleAfter`; the scan says when Hybernate is holding one awake, for example for its dependents.
- **Other workloads:** what the scan can see without Hybernate. Any of these makes a workload active: CPU at or above `--cpu-threshold` (10% by default) of what it requests, a rollout within `--idle-after` (1h by default), a `hybernate.io/last-activity` annotation within it, or a `hybernate.io/active-until` in the future. Otherwise, with CPU measured, it's idle.
- **Workloads it can't judge**, because they set no CPU requests or have no metrics yet, are listed in the notes rather than the table.
- **Replicas set from Git:** a workload whose replicas Argo CD or Flux last set, from the field managers the API server records, is listed in the notes by tool: its tool would undo every pause until it's told to leave the replicas to Hybernate, a one-time setting given in [Argo CD and Flux](../guides/gitops.md). JSON gives it in each workload's `replicasFromGit`.

**History from Prometheus:** when the cluster has a Prometheus, the scan replays Hybernate's activity clock over the last week of each workload's CPU and rollouts, as if Hybernate had managed it with `--idle-after` and `--cpu-threshold`:

- It finds Prometheus by the Services the Prometheus Operator (and so kube-prometheus-stack) and the community Helm chart create, and queries it through the API server's service proxy, so there's no port-forward or URL to give. Pass `--prometheus-url` for one outside the cluster, such as Thanos or Mimir; see [A Prometheus elsewhere](#a-prometheus-elsewhere).
- It reads CPU per container from cAdvisor's `container_cpu_usage_seconds_total`, which every common setup scrapes, at five-minute steps (coarser for windows over about 34 days), and matches pods to their workload by name. Sidecars added at pod creation are left out of activity, as for the activity clock.
- A step counts as active when the workload's own CPU is at or above the threshold of what its running pods request, or it rolled out. After `--idle-after` with none, it would be asleep until the next, which is one wake.
- **STATE** is right now: idle means no activity for `--idle-after`, so Hybernate would pause it now. Each money and history cell has one source:
    - **COULD SLEEP** and **WAKES** are what Hybernate measured for a workload in dry-run, since dry-run started (BECAUSE says when), and for an unmanaged workload come from the replay: had Hybernate been managing it, the running hours it would have been paused, after waiting `--idle-after` from each activity, and how many times activity would have arrived while it was asleep, each starting it again and making whoever caused it wait.
    - **SAVED THIS MONTH**, for live workloads, is what Hybernate has freed pausing it since the 1st, from the ManagedWorkload's `status.cost`, priced at the rates Hybernate uses for the workload.
    - **COULD SAVE/MO**, for workloads Hybernate doesn't pause yet, is what pausing would free a month: what Hybernate measured for one in dry-run, the same figure its BECAUSE line gives, or what the replay estimates for an unmanaged one.
- A live workload isn't replayed: while it's paused, there are no pods for Prometheus to record. Its pauses are what SAVED THIS MONTH counts.
- Requests and activity annotations aren't recorded in Prometheus, so the replay only knows CPU and rollouts, and a workload used with little CPU can look like it would sleep more than it would. Requests and prices are today's.
- If Prometheus keeps less than the window, the replay covers what it keeps, and the report says so. Without Prometheus, or without permission to reach it, the scan judges from CPU right now, and says why.

**Dependencies:** the scan reads each workload's environment variables, from literal values and ConfigMaps, and lists the workloads they point at:

```
  Dependencies Hybernate wakes and holds with the workloads that need them: 3 dependencies
  preview-42   deployment/checkout-api   ->   statefulset/postgres         DATABASE_URL   declared
  preview-42   deployment/checkout-api   ->   messaging/statefulset/nats   NATS_URL       connected once Hybernate manages it
  preview-42   deployment/worker         ->   statefulset/postgres         PGHOST         connected once Hybernate manages it
```

- An address counts when it names a Service in a scanned namespace, in any form cluster DNS gives it (`postgres`, `postgres.preview-42`, `postgres.preview-42.svc.cluster.local`, or a headless Service's pod, `postgres-0.postgres-hl`), inside a URL, a `host:port`, or a list of either. The workload is the one the Service's selector picks. A bare word on its own, such as `MODE=postgres`, isn't taken for an address.
- Variable names don't matter, and Service names come from the cluster, not a list the scan keeps.
- Credentials are hidden: everything before the last `@` in an address is shown as `***`, as are the values of keys such as `password`, `token`, `secret`, `key` or `auth` in `key=value` form. Query strings, which can hold credentials, are dropped, and long addresses are cut to 80 characters. The terminal shows the variable each dependency was found in; only JSON and YAML carry the address itself.
- Hybernate wakes a dependency with the workload that needs it, so the first request doesn't wait for it too, and keeps it up while that workload is; see [Dependencies](../concepts/dependencies.md). The status says whether that's in place: **declared** with `hybernate.io/depends-on`, **connected** by Hybernate, **connected once Hybernate manages** the workload, or **not connected yet**, for one Hybernate manages but hasn't applied it to.
- Secrets are never read, so addresses set there, and ones built in code or read from files, aren't found. The notes say which workloads take variables from Secrets.

**What the numbers mean:**

- Costs are what the workloads' requests cost while running, sidecars included, at the on-demand list price of the nodes each one's pods run on, from their instance type and region; see [Prices](../concepts/cost-tracking.md#prices). A workload with no pod on a node is priced where Hybernate last saw it run, or at the cluster's most common node type. Nodes Hybernate has no list price for, and every workload when your access doesn't allow listing nodes, use assumed prices from AWS on-demand in us-east-1, and the report says which.
- `--cpu-price` and `--memory-price` set your own prices, which price every workload in place of list prices: use them for discounts, savings plans, or negotiated rates.
- Spot nodes are priced at on-demand, and the report says how many workloads run on them, since they cost less than shown.
- Without history, the scan shows what idle workloads cost and what each hour asleep frees, not a monthly saving: from one moment it can't tell how often a workload would be woken. To measure savings once Hybernate is installed, label workloads `hybernate.io/managed=true` and annotate them `hybernate.io/dry-run=true`: Hybernate measures how often it would have paused them, for how long, and what that would have freed, without ever pausing them. The scan then shows it in each one's COULD SLEEP, WAKES, and COULD SAVE/MO, with BECAUSE saying since when, a total in the headline, and the `kubectl hybernate enable` command to start pausing. It's from Hybernate's own activity clock, which sees more than the history replay, so it's what to judge going live by.
- An hour asleep frees capacity; it becomes money when your cluster autoscaler removes it.
- BECAUSE gives the evidence for the state, then facts such as when a workload was last deployed, if a week or more ago.

**The HTML report:** run in a terminal, the scan also writes the same report as a web page and opens it in your browser. It's one self-contained file, with no scripts, fonts, or images loaded from anywhere (its sorting and filtering are built in), so it can be emailed or attached, and it prints cleanly to PDF. It's written for whoever you send it to:

- two headline figures: what Hybernate has saved this month, and what pausing could save a month (measured for dry-run workloads, estimated from history for unmanaged ones), as a share of what those workloads cost, "over the same time" when history was replayed, since a replayed workload is measured against what it cost over the history, with the pods it ran then; without either, what idle workloads cost while running
- four facts: what the workloads cost, what's idle right now, how long unmanaged workloads would have slept (with history) or what idle workloads cost an hour (without), and how many Hybernate manages
- a table by namespace (with a filter), every workload with the same columns as the terminal (sortable, with a filter), and the dependencies found
- what the numbers mean, in plain words, and the next steps

Dependency addresses are left out of the page, since it's meant to be passed around; JSON and YAML keep them. The report file is created readable only by you. A scan covers one cluster, the current kubeconfig context or the one you name with `--context`.

The file goes to your temporary directory unless you pass `--html FILE`. It opens only when the table is shown in a terminal on a machine with a desktop; piped output, `-o json`, CI, and SSH sessions get no report unless you ask for one with `--html`. `--open=false` keeps it from opening.

**Cluster names:** EKS and GKE contexts are shortened to the cluster with its provider and region, such as `staging (EKS us-east-1)` for `arn:aws:eks:us-east-1:123456789012:cluster/staging`. Any other context is shown as named. JSON and YAML keep the full context name.

**STATE** is what the workload is doing right now, and the bracket how Hybernate is involved: **unmanaged**, not at all; **dry-run**, it measures but never pauses; **live**, it pauses the workload while idle; **protected**, its namespace is labelled `hybernate.io/protected`, so Hybernate won't manage it, and it's left out of what pausing could save and of the next steps. The operator's `protectedNamespaces` patterns aren't visible to the scan, which only sees the label. A workload someone scaled to zero shows as `paused (unmanaged)`: it costs nothing now, but nothing will wake it on a request, and the headline counts it apart from what Hybernate has paused. Workloads labelled `hybernate.io/ignore=true` aren't listed.

```bash
# Another cluster in your kubeconfig
kubectl hybernate scan --context staging

# Some namespaces, as JSON or YAML, at your own prices
kubectl hybernate scan -n preview-42 -n preview-918 -o json --cpu-price 0.045 --memory-price 0.006

# A month of history from a Thanos that holds many clusters, with a 2h idleAfter
kubectl hybernate scan --window 30d --idle-after 2h --prometheus-url https://thanos.example.com \
  --prometheus-selector 'cluster="staging"'

# CPU right now only
kubectl hybernate scan --window 0

# Save the report to send around
kubectl hybernate scan --html workload-scan.html

# The terminal only
kubectl hybernate scan --open=false
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--namespace` | `-n` | all you can read | Namespace to scan; repeat for several |
| `--all-namespaces` | `-A` | | Read every namespace, and fail rather than fall back when your access doesn't allow that. Can't be combined with `-n` |
| `--exclude-namespaces` | | `kube-system`, `kube-public`, `kube-node-lease` | Namespaces to skip |
| `--output` | `-o` | `table` | `table`, `json`, or `yaml` |
| `--limit` | | `25` | Workloads listed in the table, idle first, then paused, then active, each by what pausing could save; `0` for all. Also caps the dependency list. JSON and YAML list everything |
| `--cpu-threshold` | | `10` | CPU use, as a percentage of requests, at which a workload counts as active, now and in the replay. Workloads Hybernate manages use their own setting |
| `--cpu-price` | | `0.031` | Your price per vCPU-hour, in dollars |
| `--memory-price` | | `0.004` | Your price per GiB-hour of memory, in dollars |
| `--window` | | `7d` | How much Prometheus history to replay, such as `7d` or `36h`; `0` judges from CPU right now only |
| `--idle-after` | | `1h` | How long without activity makes a workload idle, now and in the replay, as Hybernate's `idleAfter`. Workloads Hybernate manages use their own setting |
| `--prometheus-url` | | found in the cluster | Prometheus API to read history from, such as Thanos or Mimir |
| `--prometheus-selector` | | | Label matchers added to every history query, such as `'cluster="prod"'`, for a Prometheus that holds more than one cluster. Values are double-quoted, as in PromQL |
| `--prometheus-header` | | | Header to send to `--prometheus-url`, as `"Name: value"`, such as `"X-Scope-OrgID: tenant"` for Mimir; repeat for several |
| `--prometheus-bearer-token-file` | | | File holding a bearer token to send to `--prometheus-url` |
| `--prometheus-ca-file` | | | PEM file of CA certificates to trust for `--prometheus-url`, besides the system's |
| `--prometheus-insecure-skip-verify` | | `false` | Don't verify `--prometheus-url`'s certificate |
| `--timeout` | | `5m` | How long the scan may take before it gives up |
| `--html` | | a temporary file | Save the HTML report to this file, to share |
| `--open` | | `true` | Open the HTML report in your browser, when the table is shown in a terminal |

### A Prometheus elsewhere

`--prometheus-url` reads history from a Prometheus API your machine can reach, rather than one found in the cluster, and needs no cluster permission at all. If it doesn't answer like a Prometheus API, the scan fails rather than carrying on without history.

- **Several clusters in one store**, such as Thanos or Mimir: pass `--prometheus-selector` with the label that tells them apart, so the scan reads only this cluster's series. When it finds series from more than one source, the notes suggest it.
- **Authentication**: `--prometheus-header` (for example `X-Scope-OrgID` for a Mimir tenant, or an `Authorization` header), or `--prometheus-bearer-token-file`, but not both for the same header. `--prometheus-ca-file` and `--prometheus-insecure-skip-verify` are for its certificate. These flags only apply with `--prometheus-url`: a Prometheus found in the cluster is reached through the API server with your kubeconfig.
- **Amazon Managed Service for Prometheus** and **Google Cloud Managed Service for Prometheus** need requests signed with SigV4 or OAuth, which the scan doesn't do: point `--prometheus-url` at a signing or authenticating proxy in front of them.

The source is shown in the report with any password in its URL hidden.

### Exit status

The scan exits 0 when it read everything your access allows. Namespaces or resources your access doesn't allow (a `403`) are explained in the notes, and don't change the exit status, unless you named the namespace with `-n`.

It exits 1, after writing the report, when it couldn't read something it should have: a timeout, throttling, a server error, a failed Prometheus query, or a `403` or missing namespace among those you named with `-n`. The notes say what was missed, and JSON and YAML list the namespaces in `incomplete`. JSON and YAML also give the number of namespaces read in `namespaces`, and `"workloads": []` for a cluster with none.

Workloads someone else already scaled to zero count toward nothing the scan says could be saved: they cost nothing now, and the headline counts them apart.

### Permissions

Your user needs to read Deployments, StatefulSets, ReplicaSets, Pods, and pod metrics in the namespaces scanned, and to list namespaces unless you name them with `-n`. The standard `view` role covers it. Reading history through the service proxy also needs `get` on `services/proxy` for the Prometheus Service, which `view` doesn't include. Without it, the scan judges from CPU right now and prints what an admin can run to let you, for that one Service and nothing else:

```
To replay history, an admin can let you read Prometheus, and nothing else, with:
  kubectl create role hybernate-scan -n monitoring --verb=get --resource=services/proxy --resource-name=prometheus-operated:web
  kubectl create rolebinding hybernate-scan -n monitoring --role=hybernate-scan --user=jane@example.com
Or pass --prometheus-url if Prometheus is reachable from your machine.
```

## See What Hybernate Is Doing

```bash
kubectl hybernate status
```

```
staging (EKS us-east-1): Hybernate manages 4 workloads.
  1 paused, 2 running, 1 resuming; 1 in dry-run, measured but never paused
  Saved $42.45 this month.

Needs attention:
  preview-7/worker   GitOpsConflict   Argo CD set the replicas from Git, undoing the pause; ...
  preview-7/worker   Stuck            resuming for 25m: waiting for preview-42/postgres

  NAMESPACE    WORKLOAD               STATE               FOR   LAST ACTIVITY      NEXT                             SAVED THIS MONTH
  preview-42   deployment/api         paused              3h    request, 4h ago    wakes on a request or activity   $12.40
  preview-42   statefulset/postgres   running             5h    cpu, 2h ago        held awake by its dependents     $30.05
  preview-7    deployment/web         running (dry-run)   2h    rollout, 15m ago   would pause in 45m               -
  preview-7    deployment/worker      resuming            25m   -                  -                                -

Recent pauses and wakes:
  30m ago   preview-7/worker   scaled up to 2 replicas by argocd-controller outside Hybernate; counted as activity
  3h ago    preview-42/api     paused
  5h ago    preview-42/api     request on Service api, waking
```

`status` is one screen of every workload Hybernate manages, read from their ManagedWorkloads and events:

- **The summary:** how many workloads are in each phase, how many are in dry-run, and what Hybernate has saved this month pausing live ones.
- **Needs attention:** what keeps Hybernate from doing its job, from each workload's conditions: a GitOps tool undoing pauses (`GitOpsConflict`), a dependency that's missing or in a cycle, two ManagedWorkloads for one target, a protected namespace, a target that's gone, metrics or Prometheus that can't be read, a paused workload whose requests can't wake it (`WakeOnRequest`), and a pause or wake taking over 10 minutes (`Stuck`), with what it waits for. `kubectl describe managedworkload` has the rest.
- **The table:** each workload as `kind/name`, with its ManagedWorkload's name in brackets when you wrote one under another name; its state and for how long; its last activity and where it came from; what it has saved this month, where a saving last brought up to date in an earlier month counts as none; and **NEXT**:
    - when it pauses: `pauses in 45m`, `pauses now` once it's `Idle`, and `would pause …` in dry-run. Status is written every few minutes, so for up to 7 minutes after the time it showed, a workload still running reads `pauses soon`; after that, `overdue to pause`, which means something the table can't see holds it, and `kubectl describe managedworkload` says what
    - why it won't pause: `kept running by desiredState`, `never pauses: no idlePolicy`, `never pauses: namespace protected`, `nothing: labelled hybernate.io/ignore`, `nothing: workload not found`, `nothing: another ManagedWorkload has it`, `held awake by its dependents`, `held awake for 2h` (`hybernate.io/active-until` on the ManagedWorkload or the workload), `held awake by the forecast`, `won't pause: dependsOn cycle`, `won't pause: no CPU metrics`, or `won't pause: Prometheus failing`
    - for a paused one, what wakes it: `wakes on a request or activity`, or `wakes on activity` without the doorman; `kept paused by desiredState` when nothing will
- **Recent pauses and wakes,** newest first, as far back as the cluster keeps events, or `--since`: each pause, and each wake with its cause, a request on a Service, an activity annotation (including `kubectl hybernate wake` and a dependent waking), the forecast, or a scale-up from outside Hybernate. Requests the doorman closed without reaching the workload are listed too, and a wake whose cause wasn't recorded, such as a `desiredState: Running`, is listed by its `resumed` event alone. The table shows the latest 10; `-o json` has every one. The API server keeps events for an hour unless its `--event-ttl` says otherwise, so on most clusters that's what `status` can show.

```bash
kubectl hybernate status --context staging -n preview-42
kubectl hybernate status --since 1h -o json
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--namespace` | `-n` | all you can read | Namespace to show; repeat for several |
| `--all-namespaces` | `-A` | | Read every namespace, and fail rather than fall back when your access doesn't allow that. Can't be combined with `-n` |
| `--output` | `-o` | `table` | `table`, `json`, or `yaml` |
| `--since` | | `24h` | How far back to list pauses and wakes |
| `--timeout` | | `1m` | How long to wait for the cluster before giving up |

Your user needs `list` on `managedworkloads` and `events`, in every namespace or the ones passed with `-n`. Without access to events, it shows the rest and says so.

## Wake a Workload

```bash
kubectl hybernate wake my-api -n staging
```

```
waking staging/my-api...
staging/my-api is Running after 23s
```

`wake` marks the ManagedWorkload as active now by setting its `hybernate.io/last-activity` annotation, the same [activity annotation](../concepts/idle-detection.md#activity-annotations) a developer portal would set. A paused workload wakes, along with the workloads it [depends on](../concepts/dependencies.md); a running one has its idle clock restarted. It then waits until the workload is Running.

```bash
# Keep it awake for the next two hours, e.g. for a demo
kubectl hybernate wake my-api -n staging --for 2h

# Request the wake and return straight away
kubectl hybernate wake my-api -n staging --wait=false
```

NAME is the workload, as `status` shows it, such as `api` or `statefulset/postgres`, or its ManagedWorkload's name. When a bare name answers to more than one ManagedWorkload, such as when a Deployment and a StatefulSet share it, or one ManagedWorkload is named after a workload another one manages, it lists them and asks for the workload as `kind/name`, which only matches workloads.

What it says depends on the phase:

| Phase | What happens |
|-------|--------------|
| `Paused` | It wakes, and `wake` waits until it's `Running` |
| `Pausing` | It wakes once the pause finishes |
| `Resuming` | It's already waking |
| `Idle` | Marked active, so it stays up |
| `Running` | Already running; its idle clock restarts, and `wake` returns at once |
| dry-run, not paused | Never paused; its idle clock restarts, and `wake` returns at once |

It refuses, and says why, when activity can't wake the workload: `desiredState` is set to something other than `Running` (remove it or set it to `Running`), the workload doesn't exist or is labelled `hybernate.io/ignore`, or another ManagedWorkload manages it.

If the workload isn't `Running` within `--timeout`, `wake` exits 1 with its phase and the `kubectl describe` command that says why.

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--namespace` | `-n` | kubeconfig context's | Namespace of the workload |
| `--for` | | | Also keep the workload awake for this long, by setting `hybernate.io/active-until` |
| `--wait` | | `true` | Wait until the workload is Running |
| `--timeout` | | `5m` | How long to wait, cluster calls included |

Your user needs `get`, `list` and `patch` on `managedworkloads` in the namespace.

## Show a Workload's Dependencies

```bash
kubectl hybernate deps postgres -n preview-42
```

```
preview-42/postgres (StatefulSet, Paused)
Depends on:
  nothing
Depended on by:
  preview-42/api      learned from PGHOST                   Running
  preview-42/worker   declared in dependsOn, waitForReady   Paused
Learned links come from the dependent's environment or its requests; hybernate.io/ignore-dependencies drops one.
```

`deps` shows what Hybernate holds awake and wakes with a ManagedWorkload, in both directions: what it depends on, and what depends on it, in any namespace. Each link says where it comes from, a `dependsOn` or what Hybernate [learned](../concepts/dependencies.md#learned-dependencies) from the dependent's environment, and what the other workload is doing now; one Hybernate doesn't manage shows as `not managed`. It only reads ManagedWorkloads. Without access to them in every namespace, it reads the workload's own and says that dependents elsewhere aren't shown. NAME is found as for [`wake`](#wake-a-workload).

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--namespace` | `-n` | kubeconfig context's | Namespace of the workload |
| `--timeout` | | `1m` | How long to wait for the cluster before giving up |

## Enable a Workload

A workload in dry-run, whether from its own `hybernate.io/dry-run: "true"`, its namespace's, the cluster default, or `spec.dryRun` on a ManagedWorkload you wrote, is measured but never paused. `enable` ends that, so Hybernate starts pausing it while idle:

```bash
kubectl hybernate enable checkout-api -n preview-42
kubectl hybernate enable statefulset/postgres -n preview-42
kubectl hybernate enable --all -n preview-42
```

```
deployment/checkout-api: dry-run ended, Hybernate will pause it while idle
```

For a workload opted in with the `hybernate.io/managed` label, it sets the workload's own `hybernate.io/dry-run` annotation to `"false"`, which wins over its namespace's annotation and the cluster's default. It reads the workload's ManagedWorkload to tell whether it's in dry-run, so a dry-run from the cluster default counts too. For a ManagedWorkload you wrote, it sets `spec.dryRun: false`. It doesn't wait: Hybernate pauses the workload once its clock runs out.

`--all` sets the namespace's annotation to `"false"` instead, drops the workloads' own annotations, and sets `spec.dryRun: false` on ManagedWorkloads written by hand in the namespace.

When Argo CD or Flux applies the workload, its namespace, or the ManagedWorkload, `enable` changes nothing there, because the tool would put the old value straight back. It names the tool, prints the change to make in Git, and exits non-zero:

```
deployment/checkout-api is managed by Argo CD application shop, which would undo a change made here. In its manifest:
  set the annotation  hybernate.io/dry-run: "false"
Or pass --force to change the cluster anyway.
```

A bare name is looked up as a Deployment first, then a StatefulSet; prefix it with `deployment/` or `statefulset/` to be exact.

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--namespace` | `-n` | kubeconfig context's | Namespace of the workload |
| `--all` | | `false` | Enable every workload in the namespace |
| `--force` | | `false` | Change the cluster even when Argo CD or Flux manages the object |
| `--timeout` | | `1m` | How long to wait for the cluster before giving up |

Your user needs `get` and `patch` on the workload and `get` on its namespace. With `--all`, it also needs `list` on Deployments and StatefulSets, and `patch` on the namespace to end its dry-run. See [Opting In](../guides/opt-in.md) for the label and every setting.
