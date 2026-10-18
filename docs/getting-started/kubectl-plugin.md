# kubectl Plugin

The `kubectl hybernate` plugin scans a cluster for idle workloads, shows what Hybernate is doing in it, ends dry-run for workloads you've opted in, pauses workloads on demand, and wakes paused ones.

`scan` and `status` read every namespace you can, and fall back to your kubeconfig context's namespace, with a note, when your access doesn't allow listing them all; `-n` names namespaces instead, and `-A` (`--all-namespaces`) insists on every one, failing rather than falling back. `pause`, `wake`, `deps` and `enable` work in one namespace: `-n`, or your context's, as kubectl does.

Every command takes kubectl's connection flags: `--kubeconfig`, `--context`, `--cluster`, `--user`, `--as`, `--as-group`, `--as-uid`, `--token`, `--server`, `--certificate-authority`, `--client-certificate`, `--client-key`, `--insecure-skip-tls-verify`, `--tls-server-name`, `--proxy-url`, `--username`, `--password`, `--disable-compression` and `--request-timeout`. Each request to the API server gives up after 30 seconds unless `--request-timeout` sets otherwise (`0` keeps the 30 seconds, unlike kubectl, where it means no limit), so a cluster that stops answering ends a command with an error, not a hang. `kubectl hybernate version` prints the plugin's version.

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
- **Workloads it can't judge**, because they set no CPU requests or have no metrics yet, are listed in the notes rather than the table, and left out of the headline's totals. Without metrics-server, as on EKS by default, an unmanaged workload with CPU history is judged from it instead: its state is where replaying its history left the activity clock, and BECAUSE says so.
- **Replicas set from Git:** a workload whose replicas a GitOps tool (Argo CD, Flux, Fleet, kapp or Kluctl) last set, from the field managers the API server records, is listed in the notes by tool: its tool would undo every pause until it's told to leave the replicas to Hybernate, a one-time setting given in [GitOps tools](../guides/gitops.md). JSON gives it in each workload's `replicasFromGit`. The next steps never suggest one, since its tool would revert a label put on by hand too.
- **The cluster's own infrastructure** is left out of the table, the totals and the next steps, and a note says how many workloads in which namespaces. JSON and YAML list them in `platform`, each with what it is. Two kinds are left out, for different reasons:
    - **What Hybernate never pauses:** Kubernetes' namespaces (`kube-*`, and `openshift-*`, `cattle-*`, `gke-*` and `gmp-*`), the namespace Hybernate runs in, found by its Deployment's `app.kubernetes.io/name: hybernate` label, and, in any namespace, the operator's and the doorman's own Deployments, found by their `control-plane: controller-manager` or `control-plane: doorman` label with `app.kubernetes.io/name: hybernate` or a `hybernate` `helm.sh/chart` label. Apps' dependencies on these aren't listed, as Hybernate never needs to wake them.
    - **What the scan guesses nobody means to pause:** well-known infrastructure such as ingress controllers, cert-manager, Vault, Argo CD, Flux, service meshes, policy engines and monitoring, by their usual namespaces (`ingress-nginx`, `cert-manager`, `vault`, `argocd`, `flux-system`, `istio-system`, `linkerd`, `gatekeeper-system`, `kyverno`, `monitoring` and others) or their `app.kubernetes.io/name`, `app.kubernetes.io/part-of`, `app`, `k8s-app` or `helm.sh/chart` labels. This is only the estimate's exclusion, not an operator rule: opted in, Hybernate pauses these like any app, so apps' dependencies on them are still listed, and none is guessed at in a namespace labelled `hybernate.io/managed=true`, such as a preview environment with its own Vault, or for a workload Hybernate already manages.

    A namespace labelled `hybernate.io/allow-protected=true` is scanned like any other, apart from Hybernate's own Deployments.

**History from Prometheus:** when the cluster has a Prometheus, the scan replays Hybernate's activity clock over the last week of each workload's CPU and rollouts, as if Hybernate had managed it with `--idle-after` and `--cpu-threshold`:

- It finds Prometheus by the Services the Prometheus Operator (and so kube-prometheus-stack) and the community Helm chart create, and queries it through the API server's service proxy, so there's no port-forward or URL to give. Pass `--prometheus-url` for one outside the cluster, such as Thanos or Mimir; see [A Prometheus elsewhere](#a-prometheus-elsewhere).
- It reads CPU per container from cAdvisor's `container_cpu_usage_seconds_total`, which every common setup scrapes, at five-minute steps (coarser for windows over about 34 days), and matches pods to their workload by name. Sidecars added at pod creation are left out of activity, as for the activity clock.
- A step counts as active when the workload's own CPU is at or above the threshold of what its running pods request, or it rolled out. After `--idle-after` with none, it would be asleep until the next, which is one wake.
- **STATE** is right now: idle means no activity for `--idle-after`, so Hybernate would pause it now. Each money and history cell has one source:
    - **COULD SLEEP** and **WAKES** are what Hybernate measured for a workload in dry-run, since dry-run started (BECAUSE says when), and for an unmanaged workload come from the replay: had Hybernate been managing it, the running hours it would have been paused, after waiting `--idle-after` from each activity, and how many times activity would have arrived while it was asleep, each starting it again and making whoever caused it wait.
    - **SAVED THIS MONTH**, for live workloads, is what Hybernate has freed pausing it since the 1st, from the ManagedWorkload's `status.cost`, priced at the rates Hybernate uses for the workload. A workload put back in dry-run keeps what it saved while live; dry-run's would-be pauses never add to it.
    - **COULD SAVE/MO**, for workloads Hybernate doesn't pause yet, is what pausing would free a month: what Hybernate measured for one in dry-run, the same figure its BECAUSE line gives, or what the replay estimates for an unmanaged one.
- A live workload isn't replayed: while it's paused, there are no pods for Prometheus to record. Its pauses are what SAVED THIS MONTH counts.
- A Deployment's history is its pods' CPU, including pods of ReplicaSets since deleted, which are known by the names Kubernetes gives them, `<deployment>-<hash>-<suffix>`. A Deployment whose name is long enough for Kubernetes to cut its pods' names short has only its current ReplicaSets' pods replayed.
- Requests and activity annotations aren't recorded in Prometheus, so the replay only knows CPU and rollouts, and a workload used with little CPU can look like it would sleep more than it would. Requests and prices are today's.
- History gets the first 60% of `--timeout`, so a Prometheus or Thanos too slow for the window can't use up the whole scan. Each namespace's reads from the API server come before its history query and share that time, and a namespace reached once it's spent isn't queried at all. A namespace's query may take whatever is left of that time, however long, and isn't cut short by `--request-timeout`. Namespaces whose history isn't read in that time are judged from CPU right now, and the notes name them and say a longer `--timeout` gives history more time, or `--window 0` skips it. When Prometheus, or the API server's proxy in front of it, gives up on a query by itself with time left, a longer `--timeout` wouldn't help: the notes name those namespaces and suggest a shorter `--window`.
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
- An address is shown as its scheme, host and port alone, such as `postgres://postgres:5432`: a user, a password, a path and a query string can each hold a credential, so none of them is. The terminal shows the variable each dependency was found in; only JSON and YAML carry the address itself.
- Hybernate wakes a dependency with the workload that needs it, so the first request doesn't wait for it too, and keeps it up while that workload is; see [Dependencies](../concepts/dependencies.md). The status says whether that's in place: **declared** with `hybernate.io/depends-on`, **connected** by Hybernate, **connected once Hybernate manages** the workload, or **not connected yet**, for one Hybernate manages but hasn't applied it to.
- Secrets are never read, so addresses set there, and ones built in code or read from files, aren't found. The notes say which workloads take variables from Secrets.

**What the numbers mean:**

- Costs are what the workloads' requests cost while running, sidecars included, at the on-demand list price of the nodes each one's pods run on, from their instance type and region; see [Prices](../concepts/cost-tracking.md#prices). A workload with no pod on a node is priced where Hybernate last saw it run, or at the cluster's most common node type. Nodes Hybernate has no list price for, and every workload when your access doesn't allow listing nodes, use assumed prices from AWS on-demand in us-east-1, and the report says which.
- `--cpu-price` and `--memory-price` set your own prices, which price every workload in place of list prices: use them for discounts, savings plans, or negotiated rates.
- Spot nodes are priced at on-demand, and the report says how many workloads run on them, since they cost less than shown.
- Without history, the scan shows what idle workloads cost and what each hour asleep frees, not a monthly saving: from one moment it can't tell how often a workload would be woken. To measure savings once Hybernate is installed, label workloads `hybernate.io/managed=true` and annotate them `hybernate.io/dry-run=true`: Hybernate measures how often it would have paused them, for how long, and what that would have freed, without ever pausing them. The scan then shows it in each one's COULD SLEEP, WAKES, and COULD SAVE/MO, with BECAUSE saying since when, a total in the headline, and the `kubectl hybernate enable` command to start pausing. It's from Hybernate's own activity clock, which sees more than the history replay, so it's what to judge going live by.
- An hour asleep frees capacity; it becomes money when your cluster autoscaler removes it.
- BECAUSE gives the evidence for the state, then facts such as when a workload was last deployed, if a week or more ago.
- A StatefulSet whose `persistentVolumeClaimRetentionPolicy.whenScaled` is `Delete` would lose its data to a pause, so Hybernate refuses to pause it: BECAUSE says `won't pause: PVCs deleted on scale-down` (JSON `wontPause`), and it's left out of what pausing could save and of next steps.

**The HTML report:** run in a terminal, the scan also writes the same report as a web page and opens it in your browser. It's one self-contained file, with no scripts, fonts, or images loaded from anywhere (its sorting and filtering are built in), so it can be emailed or attached, and it prints cleanly to PDF. It's written for whoever you send it to:

- two headline figures: what Hybernate has saved this month, and what pausing could save a month (measured for dry-run workloads, estimated from history for unmanaged ones), as a share of what those workloads cost, "over the same time" when history was replayed, since a replayed workload is measured against what it cost over the history, with the pods it ran then; without either, what idle workloads cost while running
- four facts: what the workloads cost, what's idle right now, how long unmanaged workloads would have slept (with history) or what idle workloads cost an hour (without), and how many Hybernate manages
- a table by namespace (with a filter), every workload scanned that the scan could judge, with the same columns as the terminal (sortable, with a filter), and the dependencies found
- what the numbers mean, in plain words, and the next steps

Dependency addresses are left out of the page, since it's meant to be passed around; JSON and YAML keep them. The report file is created readable only by you. A scan covers one cluster, the current kubeconfig context or the one you name with `--context`.

The file goes to your temporary directory unless you pass `--html FILE`. It opens only when the table is shown in a terminal on a machine with a desktop; piped output, `-o json`, CI, and SSH sessions (unless X forwarding gives a Linux one a display) get no report unless you ask for one with `--html`. `--open=false` keeps it from opening.

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
| `--cpu-threshold` | | `10` | CPU use, as a percentage of requests from 1 to 100, at which a workload counts as active, now and in the replay. Workloads Hybernate manages use their own setting |
| `--cpu-price` | | `0.031` | Your price per vCPU-hour, in dollars, zero or more |
| `--memory-price` | | `0.004` | Your price per GiB-hour of memory, in dollars, zero or more |
| `--window` | | `7d` | How much Prometheus history to replay, such as `7d` or `36h`, up to `366d`; `0` judges from CPU right now only |
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

It exits 1, after writing the report, when it couldn't read something it should have: a timeout, throttling, a server error, a failed Prometheus query, or a `403` or missing namespace among those you named with `-n`. The notes say what was missed, the error says what to do about it (check a namespace's name, ask for access, or try again with a longer `--timeout`), and JSON and YAML list the namespaces in `incomplete`, by why: `missing`, `denied`, `failed`, or `unscanned`. JSON and YAML also give the number of namespaces read in `namespaces`, and `"workloads": []` for a cluster with none.

A scan that runs out of `--timeout` still reports the namespaces it scanned in time, and lists the rest as `unscanned`; only one that scanned none fails without a report. Scanning every namespace, the scan lists each kind of object across the cluster at once, a page at a time, rather than in each namespace, so a cluster of thousands of namespaces takes a few dozen requests. A kind your access only lets you list namespace by namespace is read that way.

Workloads someone else already scaled to zero count toward nothing the scan says could be saved: they cost nothing now, and the headline counts them apart.

### Permissions

Your user needs to list Deployments, StatefulSets, ReplicaSets, ControllerRevisions, Pods, pod metrics, Services and ManagedWorkloads in the namespaces scanned, and to read the namespaces and the ConfigMaps workloads take variables from; to list namespaces unless you name them with `-n`; and to list nodes, to price workloads at their nodes. The standard `view` role covers the namespaced reads, ManagedWorkloads too, which Hybernate's chart and manifests add to `view`; it doesn't cover nodes, which are cluster-wide. Without one of these the scan carries on, and the notes say what it couldn't see. Reading history through the service proxy also needs `get` on `services/proxy` for the Prometheus Service, which `view` doesn't include. Without it, the scan judges from CPU right now and prints what an admin can run to let you, for that one Service and nothing else:

```
To replay history, an admin can let you read Prometheus, and nothing else, with:
  kubectl create role hybernate-scan -n monitoring --verb=get --resource=services/proxy --resource-name=prometheus-operated:web
  kubectl create rolebinding hybernate-scan -n monitoring --role=hybernate-scan --user=jane@example.com
Or pass --prometheus-url if Prometheus is reachable from your machine.
```

That's only when the API server refuses you the proxy. A `403` from behind it, such as from a `kube-rbac-proxy` in front of Prometheus, is shown as Prometheus's own answer, since a Role for `services/proxy` wouldn't change it.

## See What Hybernate Is Doing

```bash
kubectl hybernate status
```

```
staging (EKS us-east-1): Hybernate manages 4 workloads.
  1 paused, 2 running, 1 resuming; 1 in dry-run, measured but never paused
  Saved $42 this month.

Needs attention:
  preview-7/worker   GitOpsConflict   Argo CD set the replicas from Git, undoing the pause; ...
  preview-7/worker   Stuck            resuming for 25m: waiting for preview-42/statefulset/postgres

  NAMESPACE    WORKLOAD               STATE               FOR   LAST ACTIVITY      NEXT                             SAVED THIS MONTH
  preview-42   deployment/api         paused              3h    request, 4h ago    wakes on a request or activity   $12
  preview-42   statefulset/postgres   running             5h    cpu, 2h ago        held awake by its dependents     $30
  preview-7    deployment/web         running (dry-run)   2h    rollout, 15m ago   would pause in 45m               -
  preview-7    deployment/worker      resuming            25m   -                  -                                -

Recent pauses and wakes:
  30m ago   preview-7/worker   scaled up to 2 replicas by argocd-controller outside Hybernate; counted as activity
  3h ago    preview-42/api     paused
  5h ago    preview-42/api     request on Service api, waking
```

`status` is one screen of every workload Hybernate manages, read from their ManagedWorkloads and events:

- **The summary:** how many workloads are in each phase, how many are in dry-run, and what Hybernate has saved this month pausing live ones, in whole dollars as `scan` shows it, or `<$1` for a saving under 50¢.
- **Needs attention:** what keeps Hybernate from doing its job, from each workload's conditions: a GitOps tool undoing pauses (`GitOpsConflict`), a dependency that's missing, in a cycle, or in a namespace that doesn't allow it (`DependencyNotAllowed`), two ManagedWorkloads for one target, a protected namespace, a target that's gone, metrics or Prometheus that can't be read, a Prometheus query that has never matched a series (`PrometheusQueryEmpty`), a paused workload whose requests can't wake it (`WakeOnRequest`), a resume that stopped with replicas not Ready (`ResumeIncomplete`), a pause that stopped waiting with pods still running (`PauseIncomplete`), and a pause or wake taking over 10 minutes (`Stuck`), with what it waits for. `kubectl describe managedworkload` has the rest.
- **The table:** each workload as `kind/name`, with its ManagedWorkload's name in brackets when you wrote one under another name; its state and for how long; its last activity and where it came from; what it has saved this month, in whole dollars or `<$1` under 50¢, where a saving last brought up to date in an earlier month counts as none, and one in dry-run now keeps what its pauses saved before dry-run was turned on; and **NEXT**:
    - when it pauses: `pauses in 45m`, `pauses now` once it's `Idle`, and `would pause …` in dry-run. Status is written every few minutes, so for up to 7 minutes after the time it showed, a workload still running reads `pauses soon`; after that, `overdue to pause`, which means something the table can't see holds it, and `kubectl describe managedworkload` says what
    - why it won't pause: `never pauses: no idlePolicy`, `never pauses: namespace protected`, `never pauses: part of Hybernate` (the operator's or the doorman's own Deployment), `never pauses: PVCs deleted on scale-down` (a StatefulSet whose `persistentVolumeClaimRetentionPolicy.whenScaled` is `Delete`), `nothing: labelled hybernate.io/ignore`, `nothing: workload not found`, `nothing: another ManagedWorkload has it`, `held awake by its dependents`, `held awake for 2h` (`hybernate.io/active-until` on the ManagedWorkload or the workload), `held awake by the forecast`, `won't pause: dependsOn cycle`, `won't pause: no CPU metrics`, or `won't pause: Prometheus failing`
    - for a paused one, what wakes it: `wakes on a request or activity`, or `wakes on activity` without the doorman
- **Recent pauses and wakes,** newest first, as far back as the cluster keeps events, or `--since`: each pause, and each wake with its cause, a request on a Service, an activity annotation (including `kubectl hybernate wake` and a dependent waking), the forecast, or a scale-up from outside Hybernate. Requests the doorman closed without reaching the workload are listed too, and a wake whose cause wasn't recorded, such as a hand-back when a namespace becomes protected, is listed by its `resumed` event alone. The table shows the latest 10; `-o json` has every one. The API server keeps events for an hour unless its `--event-ttl` says otherwise, so on most clusters that's what `status` can show.

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

Your user needs `list` on `managedworkloads` and `events`, in every namespace or the ones passed with `-n`. Without access to events, it shows the rest and says so. It also lists `deployments` and `statefulsets`, reading only their metadata, for a `hybernate.io/active-until` annotation on the workload itself, and goes without it when it can't. A namespace named with `-n` that doesn't exist is an error, which takes `get` on `namespaces` to tell; without it, the namespace shows as one Hybernate manages nothing in.

## Pause a Workload

```bash
kubectl hybernate pause my-api -n staging
```

```
pausing staging/my-api...
staging/my-api is Paused after 4s; a request to it, or kubectl hybernate wake, wakes it
```

`pause` asks Hybernate to pause a workload now, instead of when its idle clock runs out: a preview environment at the end of the day, say. It's an ordinary pause, the same as an idle one: a request to the workload through the [doorman](../concepts/wake-on-request.md), [activity](../concepts/idle-detection.md#activity-annotations), `autoResume` or [`wake`](#wake-a-workload) wakes it. It works whether or not the workload has an `idlePolicy`. See [Pause and Resume](../guides/pause.md#pause-now) for exactly what Hybernate does with the request.

```bash
# Pause it even if the forecast expects it to be busy soon, without asking
kubectl hybernate pause my-api -n staging --yes

# Request the pause and return straight away
kubectl hybernate pause my-api -n staging --wait=false
```

NAME is found as for [`wake`](#wake-a-workload).

What it says depends on the phase:

| Phase | What happens |
|-------|--------------|
| `Running` | It pauses, and `pause` waits until it's `Paused` |
| `Idle` | It pauses now |
| `Resuming` | It pauses once it's up |
| `Pausing` | Already pausing; `pause` waits until it's `Paused` |
| `Paused` | Already paused; `pause` returns at once |
| dry-run | Never paused: Hybernate counts a would-be pause, and `pause` says so |

Hybernate pauses it by the same rules as an idle pause, so `pause` exits 1 with Hybernate's reason when it won't: workloads that are awake depend on it, a `dependsOn` cycle runs through it, its namespace is protected, or the workload doesn't exist, is labelled `hybernate.io/ignore` or is managed by another ManagedWorkload. A workload scaled to zero outside Hybernate is off already, which `pause` says, exiting 0. Asking does override the hour Hybernate waits after a GitOps tool undid its last pause.

When a confident forecast expects the workload to be busy within the hour, Hybernate declines the pause and says when, since it would likely be woken straight back, and `pause` asks:

```
preview-42/api wasn't paused: the forecast expects demand at 80% of requests in the hour from 09:00 UTC
Pause it anyway? [y/N]
```

Answering `y` asks again overriding the forecast, and it pauses; anything else leaves it running, exiting 0. The question waits for you as long as you take: `--timeout` counts only the time spent on the cluster, and starts again after your answer. `--yes` overrides the forecast from the start, without asking. With no terminal to ask on, such as in a script, `pause` exits 1 with the reason and says to pass `--yes`.

When a `hybernate.io/active-until` annotation holds it awake, on the ManagedWorkload, as `wake --for` sets, or on the workload itself, Hybernate would refuse the pause, so `pause` asks first, naming which holds it:

```
preview-42/api wasn't paused: the hybernate.io/active-until annotation on its ManagedWorkload keeps it awake until 2026-10-17T20:00:00Z
Clear it and pause now? [y/N]
```

Answering `y` removes the annotation, from both when both hold it, and pauses it; anything else leaves it running, exiting 0. `--clear-hold` clears it without asking; with no terminal to ask on and without `--clear-hold`, `pause` exits 1 and says to pass it. `--yes` doesn't clear a hold, so a nightly script that pauses with `--yes` doesn't end one someone set for a demo.

When a GitOps tool last set the workload's replicas, `pause` warns before asking, with the one-time fix: the tool sets them from Git again on its next sync, which undoes the pause. See [GitOps tools](../guides/gitops.md).

If the workload isn't `Paused`, and Hybernate hasn't said why, within `--timeout`, `pause` exits 1 with its phase and the `kubectl describe` command that says why.

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--namespace` | `-n` | kubeconfig context's | Namespace of the workload |
| `--wait` | | `true` | Wait until the workload is Paused, or Hybernate says why it isn't |
| `--yes` | `-y` | `false` | Pause it even if the forecast expects it to be busy within the hour, without asking |
| `--clear-hold` | | `false` | Clear a `hybernate.io/active-until` hold on its ManagedWorkload, as `wake --for` sets, or on the workload, without asking |
| `--timeout` | | `5m` | How long to wait, cluster calls included; it starts again after you answer a question |

Your user needs `get`, `list` and `patch` on `managedworkloads` in the namespace, and `get` on the workload for the GitOps warning and its own hold, which are left out without it. Clearing a hold on the workload takes `patch` on it.

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

It refuses, and says why, when activity can't wake the workload: the workload doesn't exist or is labelled `hybernate.io/ignore`, or another ManagedWorkload manages it.

If the workload isn't `Running` within `--timeout`, `wake` exits 1 with its phase and the `kubectl describe` command that says why.

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--namespace` | `-n` | kubeconfig context's | Namespace of the workload |
| `--for` | | | Also keep the workload awake for this long, by setting `hybernate.io/active-until` on its ManagedWorkload. A hold there that ends later stays, and `wake` says so |
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

`deps` shows what Hybernate holds awake and wakes with a ManagedWorkload, in both directions: what it depends on, and what depends on it, in any namespace. Each link says where it comes from, a `dependsOn` or what Hybernate [learned](../concepts/dependencies.md#learned-dependencies) from the dependent's environment or from a wake, and what the other workload is doing now; one Hybernate doesn't manage shows as `not managed`. It only reads ManagedWorkloads. Without access to them in every namespace, it reads the workload's own and says that dependents elsewhere aren't shown; a dependency in another namespace then shows as `unknown (can't read <namespace>)`. NAME is found as for [`wake`](#wake-a-workload).

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

`enable` only ends dry-run for a workload Hybernate manages. Otherwise it changes nothing and exits non-zero with the reason: the namespace is protected (labelled `hybernate.io/protected=true`, or a `kube-*` namespace, unless labelled `hybernate.io/allow-protected=true`); the workload is labelled `hybernate.io/ignore=true`, or its own `hybernate.io/managed` is anything but `"true"`, which opts it out of a managed namespace; no ManagedWorkload manages it, because the operator isn't running, doesn't watch the namespace, or protects it by its own settings; or its ManagedWorkload says Hybernate never pauses it: `Protected=True` (such as by a `protectedNamespaces` pattern, or for Hybernate's own operator and doorman), a target labelled `hybernate.io/ignore`, or `DuplicateTarget=True`. Right after you label a workload, the operator may not have made its ManagedWorkload yet; run `enable` again a few seconds later. Without the ManagedWorkload CRD, it says Hybernate isn't installed.

`--all` ends dry-run for every workload in the namespace. When the namespace's own `hybernate.io/dry-run` annotation, or the cluster default, keeps any of them in dry-run, it sets the namespace's annotation to `"false"` and drops the workloads' own annotations, so the namespace decides for all of them. When each is in dry-run by its own annotation, it leaves the namespace alone and sets each workload's annotation to `"false"`. Either way, it sets `spec.dryRun: false` on ManagedWorkloads written by hand in the namespace. When Git owns the namespace, its annotation is left for you to change there, which also ends dry-run for the workloads that take it, and workloads with their own annotation get `"false"`. When your access doesn't allow changing the namespace, as with `edit` or `admin` bound in it, `enable` leaves its annotation alone, says so, and sets every workload's own annotation to `"false"`, which wins over it; with nothing but the namespace's annotation to change, it exits non-zero. Labelled workloads no ManagedWorkload manages are left alone. When `enable` just set the namespace's annotation to `"false"`, that ends their dry-run too once Hybernate picks them up, unless they have an annotation of their own, so they're covered. The rest are listed, and `enable` exits non-zero. When none of the namespace's workloads has been picked up yet, `enable` changes nothing, the namespace's annotation included, and exits non-zero, since the operator may not be running. Workloads Hybernate never pauses, by their ManagedWorkload, are left alone and listed too.

When a GitOps tool (Argo CD, Flux, Fleet, kapp or Kluctl) applies the setting `enable` would change, the `hybernate.io/dry-run` annotation on the workload or its namespace, or a ManagedWorkload's `spec.dryRun`, `enable` changes nothing there, because the tool would put the old value straight back. It tells by the field managers the API server records as owning that setting, as `pause` does for replicas, so it doesn't matter how the tool tracks what it applies; for an object with no managed fields recorded, it goes by the tracking label or annotation Argo CD or Flux adds. A tool that applies the workload but not the annotation, such as when Git doesn't have it, leaves a change to it alone. It names the tool, prints the change to make in Git, and exits non-zero:

```
deployment/checkout-api is managed by Argo CD application shop, which would undo a change made here. In its manifest:
  set the annotation  hybernate.io/dry-run: "false"
Or pass --force to change the cluster anyway.
```

When Helm set it, `enable` changes it, since Helm only applies a chart when you run it, and warns that the next `helm upgrade` puts the chart's value back, so set it in the chart too. See [GitOps tools](../guides/gitops.md#ending-dry-run).

A bare name is looked up as a Deployment and as a StatefulSet. When both have it, `enable` lists them and asks for the kind; prefix the name with `deployment/` or `statefulset/` to be exact.

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--namespace` | `-n` | kubeconfig context's | Namespace of the workload |
| `--all` | | `false` | Enable every workload in the namespace |
| `--force` | | `false` | Change the cluster even when a GitOps tool applies the setting from Git |
| `--timeout` | | `1m` | How long to wait for the cluster before giving up |

Your user needs `get` and `patch` on the workload, `get` on its namespace, and `list` on `managedworkloads` in it. Without `list` on ManagedWorkloads, `enable` goes by the annotations alone, so it can't check that Hybernate manages the workload or see a dry-run from the cluster default, and it says so. With `--all`, it also needs `list` on Deployments and StatefulSets, and `patch` on the namespace to end its dry-run there; without it, each workload gets its own annotation instead. See [Opting In](../guides/opt-in.md) for the label and every setting.
