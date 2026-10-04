# kubectl Plugin

The `kubectl hybernate` plugin scans a cluster for idle workloads, ends dry-run for workloads you've opted in, and wakes paused ones. Its commands use the namespace of your current kubeconfig context unless you pass `-n`, as kubectl does.

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

- It finds Prometheus by the Services the Prometheus Operator (and so kube-prometheus-stack) and the community Helm chart create, and queries it through the API server's service proxy, so there's no port-forward or URL to give. Pass `--prometheus-url` for one outside the cluster, such as Thanos, Mimir, or a managed Prometheus.
- It reads CPU per container from cAdvisor's `container_cpu_usage_seconds_total`, which every common setup scrapes, at five-minute steps, and matches pods to their workload by name. Sidecars added at pod creation are left out of activity, as for the activity clock.
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
- Passwords in URLs are shown as `***`, and query strings, which can hold credentials, are dropped.
- Hybernate wakes a dependency with the workload that needs it, so the first request doesn't wait for it too, and keeps it up while that workload is; see [Dependencies](../concepts/dependencies.md). The status says whether that's in place: **declared** with `hybernate.io/depends-on`, **connected** by Hybernate, **connected once Hybernate manages** the workload, or **not connected yet**, for one Hybernate manages but hasn't applied it to.
- Secrets are never read, so addresses set there, and ones built in code or read from files, aren't found. The notes say which workloads take variables from Secrets.

**What the numbers mean:**

- Costs are what the workloads' requests cost while running, sidecars included, at the on-demand list price of the nodes each one's pods run on, from their instance type and region; see [Prices](../concepts/cost-tracking.md#prices). A workload with no pod on a node is priced where Hybernate last saw it run, or at the cluster's most common node type. Nodes Hybernate has no list price for, and every workload when your access doesn't allow listing nodes, use assumed prices from AWS on-demand in us-east-1, and the report says which.
- `--cpu-price` and `--memory-price` set your own prices, which price every workload in place of list prices: use them for discounts, savings plans, or negotiated rates.
- Spot nodes are priced at on-demand, and the report says how many workloads run on them, since they cost less than shown.
- Without history, the scan shows what idle workloads cost and what each hour asleep frees, not a monthly saving: from one moment it can't tell how often a workload would be woken. To measure savings once Hybernate is installed, label workloads `hybernate.io/managed=true` and annotate them `hybernate.io/dry-run=true`: Hybernate measures how often it would have paused them, for how long, and what that would have freed, without ever pausing them. The scan then shows it in each one's COULD SLEEP, WAKES, and COULD SAVE/MO, with BECAUSE saying since when, a total in the headline, and the `kubectl hybernate enable` command to start pausing. It's from Hybernate's own activity clock, which sees more than the history replay, so it's what to judge going live by.
- An hour asleep frees capacity; it becomes money when your cluster autoscaler removes it.
- BECAUSE gives the evidence for the state, then facts such as when a workload was last deployed, if a week or more ago.

**The HTML report:** run in a terminal, the scan also writes the same report as a web page and opens it in your browser. It's one self-contained file, with no scripts, fonts, or images loaded from anywhere, so it can be emailed or attached, and it prints cleanly to PDF. It's written for whoever you send it to:

- two headline figures: what Hybernate has saved this month, and what pausing could save a month (measured for dry-run workloads, estimated from history for unmanaged ones), as a share of what the workloads cost; without either, what idle workloads cost while running
- four facts: what the workloads cost, what's idle right now, how many unmanaged workloads would have slept, and how many Hybernate manages
- a table by namespace (with a filter), every workload with the same columns as the terminal (sortable, with a filter), and the dependencies found
- what the numbers mean, in plain words, and the next steps

Dependency addresses are left out of the page, since it's meant to be passed around; the terminal and JSON keep them. A scan covers one cluster, the current kubeconfig context or the one you name with `--context`; [Hybernate Hub](https://okedeji.io/hybernate/hub) shows every cluster together.

The file goes to your temporary directory unless you pass `--html FILE`. It opens only when the table is shown in a terminal on a machine with a desktop; piped output, `-o json`, CI, and SSH sessions get no report unless you ask for one with `--html`. `--open=false` keeps it from opening.

**Cluster names:** EKS and GKE contexts are shortened to the cluster with its provider and region, such as `staging (EKS us-east-1)` for `arn:aws:eks:us-east-1:123456789012:cluster/staging`. Any other context is shown as named. JSON and YAML keep the full context name.

**STATE** is what the workload is doing right now, and the bracket how Hybernate is involved: **unmanaged**, not at all; **dry-run**, it measures but never pauses; **live**, it pauses the workload while idle. A workload someone scaled to zero shows as `paused (unmanaged)`: it costs nothing now, but nothing will wake it on a request, and the headline counts it apart from what Hybernate has paused. Workloads labelled `hybernate.io/ignore=true` aren't listed.

```bash
# Another cluster in your kubeconfig
kubectl hybernate scan --context staging

# Some namespaces, as JSON or YAML, at your own prices
kubectl hybernate scan -n preview-42 -n preview-918 -o json --cpu-price 0.045 --memory-price 0.006

# A month of history from a Prometheus outside the cluster, with a 2h idleAfter
kubectl hybernate scan --window 30d --idle-after 2h --prometheus-url https://thanos.example.com

# CPU right now only
kubectl hybernate scan --window 0

# Save the report to send around
kubectl hybernate scan --html workload-scan.html
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--context` | | current context | Kubeconfig context of the cluster to scan |
| `--namespace` | `-n` | all you can read | Namespace to scan; repeat for several |
| `--exclude-namespaces` | | `kube-system`, `kube-public`, `kube-node-lease` | Namespaces to skip |
| `--output` | `-o` | `table` | `table`, `json`, or `yaml` |
| `--limit` | | `25` | Workloads listed per cluster in the table, idle first; `0` for all |
| `--cpu-threshold` | | `10` | CPU use, as a percentage of requests, at which a workload counts as active, now and in the replay. Workloads Hybernate manages use their own setting |
| `--cpu-price` | | `0.031` | Your price per vCPU-hour, in dollars |
| `--memory-price` | | `0.004` | Your price per GiB-hour of memory, in dollars |
| `--window` | | `7d` | How much Prometheus history to replay, such as `7d` or `36h`; `0` judges from CPU right now only |
| `--idle-after` | | `1h` | How long without activity makes a workload idle, now and in the replay, as Hybernate's `idleAfter`. Workloads Hybernate manages use their own setting |
| `--prometheus-url` | | found in the cluster | Prometheus API to read history from |
| `--timeout` | | `5m` | How long the scan may take before it gives up. Each request to the API server also gives up after 30 seconds, so a cluster that stops answering ends the scan with an error, not a hang |
| `--html` | | a temporary file | Save the HTML report to this file, to share |
| `--open` | | `true` | Open the HTML report in your browser, when the table is shown in a terminal |

Your user needs to read Deployments, StatefulSets, ReplicaSets, Pods, and pod metrics in the namespaces scanned, and to list namespaces unless you name them with `-n`. The standard `view` role covers it. Reading history through the service proxy also needs `get` on `services/proxy` for the Prometheus Service, which `view` doesn't include. Without it, the scan judges from CPU right now and prints what an admin can run to let you, for that one Service and nothing else:

```
To replay history, an admin can let you read Prometheus, and nothing else, with:
  kubectl create role hybernate-scan -n monitoring --verb=get --resource=services/proxy --resource-name=prometheus-operated:web
  kubectl create rolebinding hybernate-scan -n monitoring --role=hybernate-scan --user=jane@example.com
Or pass --prometheus-url if Prometheus is reachable from your machine.
```

`--prometheus-url` needs no cluster permission at all.

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

It refuses, and says why, when activity can't wake the workload: `desiredState: Paused` (remove it or set it to `Running`).

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--namespace` | `-n` | kubeconfig context's | Namespace of the ManagedWorkload |
| `--for` | | | Also keep the workload awake for this long, by setting `hybernate.io/active-until` |
| `--wait` | | `true` | Wait until the workload is Running |
| `--timeout` | | `5m` | How long to wait |

Your user needs `get` and `patch` on `managedworkloads` in the namespace.

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

`deps` shows what Hybernate holds awake and wakes with a ManagedWorkload, in both directions: what it depends on, and what depends on it, in any namespace. Each link says where it comes from, a `dependsOn` or what Hybernate [learned](../concepts/dependencies.md#learned-dependencies) from the dependent's environment, and what the other workload is doing now; one Hybernate doesn't manage shows as `not managed`. It only reads ManagedWorkloads. Without access to them in every namespace, it reads the workload's own and says that dependents elsewhere aren't shown.

## Enable a Workload

A workload opted in with `hybernate.io/dry-run: "true"` is measured but never paused. `enable` ends that, so Hybernate starts pausing it while idle:

```bash
kubectl hybernate enable checkout-api -n preview-42
kubectl hybernate enable statefulset/postgres -n preview-42
kubectl hybernate enable --all -n preview-42
```

```
deployment/checkout-api: dry-run ended, Hybernate will pause it while idle
```

It removes the workload's `hybernate.io/dry-run` annotation. When the dry-run comes from the namespace, it sets the workload's own to `"false"` instead, overriding the namespace's. `--all` ends dry-run for every workload in the namespace and for the namespace itself.

When Argo CD or Flux applies the workload, `enable` changes nothing in the cluster, because the tool would put the annotation straight back. It names the tool and prints the change to make in Git, and exits non-zero:

```
deployment/checkout-api is managed by Argo CD application shop, which would undo a change made here. In its manifest:
  remove the annotation  hybernate.io/dry-run: "true"
Or pass --force to change the cluster anyway.
```

A bare name is looked up as a Deployment first, then a StatefulSet; prefix it with `deployment/` or `statefulset/` to be exact.

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--namespace` | `-n` | kubeconfig context's | Namespace of the workload |
| `--all` | | `false` | Enable every workload in the namespace |
| `--force` | | `false` | Change the cluster even when Argo CD or Flux manages the workload |

Your user needs `get` and `patch` on the workload and `get` on its namespace. With `--all`, it also needs `list` on Deployments and StatefulSets, and `patch` on the namespace to end its dry-run. See [Opting In](../guides/opt-in.md) for the label and every setting.
