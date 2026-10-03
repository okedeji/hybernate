# kubectl Plugin

The `kubectl hybernate` plugin scans your clusters for idle workloads, ends dry-run for workloads you've opted in, and wakes paused ones. Its commands use the namespace of your current kubeconfig context unless you pass `-n`, as kubectl does.

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

## Scan Your Clusters

```bash
kubectl hybernate scan
```

```
staging (EKS us-east-1): 214 workloads in 38 namespaces

  Hybernate has 12 workloads paused right now, freeing $1.84 an hour.
  61 workloads are idle right now, reserving 48.0 vCPU and 190.0 GiB of memory.
  They cost $3,180/month while running, $4.36 an hour: each hour asleep frees that.
  Replaying the last 7 days, Hybernate would have paused 58 workloads for 6,120 hours in all, waking them 214 times,
  and freed $1,240: about $5,380/month.

  NAMESPACE    WORKLOAD               STATE              CPU   REPLICAS   COST/MO   IDLE           ASLEEP   WAKES   FREES/MO   WHY
  sandbox-42   statefulset/postgres   idle               0%    1          $140      168h of 168h   167h     0       $139       CPU 0%; not deployed in 23 days
  sandbox-7    deployment/api         idle (dry-run)     1%    2          $96       131h of 168h   112h     9       $64        no activity for 5h
  sandbox-3    deployment/web         paused (managed)   -     2          $48       -              -        -       -          paused 3h ago
  payments     deployment/ledger      active             64%   3          $210      12h of 168h    0h       0       $0         CPU 64%
  ...

Notes:
  - the history replay sees CPU and rollouts only; requests and activity annotations aren't in Prometheus, so a workload used with little CPU can look like it would sleep more than it would
  - 2 workloads set no CPU requests, so their use can't be measured: kube-tools/agent, ops/exporter
```

`scan` only reads, with your own kubeconfig, and needs nothing installed in the cluster. It works before you install Hybernate, and after: once Hybernate manages a workload, the scan shows what it has paused and reads its activity clock.

**How it decides a workload is idle**, using the same signals Hybernate uses to pause:

- **Workloads Hybernate manages:** the activity clock it keeps for them, which already combines every source configured, including Prometheus queries. Idle means no activity for `idleAfter`; the scan says when Hybernate is holding one awake, for example for its dependents.
- **Other workloads:** what the scan can see without Hybernate. Any of these makes a workload active: CPU at or above 10% of what it requests, a rollout in the last hour, a `hybernate.io/last-activity` annotation in the last hour, or a `hybernate.io/active-until` in the future. Otherwise, with CPU measured, it's idle.
- **Workloads it can't judge**, because they set no CPU requests or have no metrics yet, are listed in the notes rather than the table.

**History from Prometheus:** when the cluster has a Prometheus, the scan replays Hybernate's activity clock over the last week of each workload's CPU and rollouts, as if Hybernate had managed it with `idleAfter: 1h`:

- It finds Prometheus by the Services the Prometheus Operator (and so kube-prometheus-stack) and the community Helm chart create, and queries it through the API server's service proxy, so there's no port-forward or URL to give. Pass `--prometheus-url` for one outside the cluster, such as Thanos, Mimir, or a managed Prometheus.
- It reads CPU per container from cAdvisor's `container_cpu_usage_seconds_total`, which every common setup scrapes, at five-minute steps, and matches pods to their workload by name. Sidecars added at pod creation are left out of activity, as for the activity clock.
- A step counts as active when the workload's own CPU is at or above the threshold of what its running pods request, or it rolled out. After `--idle-after` with none, it would be asleep until the next, which is one wake.
- **IDLE** is the running hours below the threshold; **ASLEEP** is the running hours it would have been paused; **WAKES** is how many times it would have been woken; **FREES/MO** is what its pods cost during ASLEEP, over an average month at the same rate.
- The headline sums workloads Hybernate doesn't pause yet: unmanaged ones and those in dry-run. Managed workloads are replayed too, for comparison, but not counted again.
- Requests and activity annotations aren't recorded in Prometheus, so the replay only knows CPU and rollouts, and a workload used with little CPU can look like it would sleep more than it would. Requests and prices are today's.
- If Prometheus keeps less than the window, the replay covers what it keeps, and the report says so. Without Prometheus, or without permission to reach it, the scan judges from CPU right now, and says why.

**Dependencies:** the scan reads each workload's environment variables, from literal values and ConfigMaps, and lists the workloads they point at:

```
  Dependencies found in environment variables (suggestions; never applied):
  sandbox-42   deployment/checkout-api   ->   statefulset/postgres         DATABASE_URL=postgres://shop:***@postgres:5432/shop   already declared
  sandbox-42   deployment/checkout-api   ->   messaging/statefulset/nats   NATS_URL=nats://nats.messaging:4222
  sandbox-42   deployment/worker         ->   statefulset/postgres         PGHOST=postgres-0.postgres-hl                          headless
  The doorman can't hold connections to a headless address, so a request won't wake what's behind it.
  Add the dependency to the workloads that use one:
    sandbox-42/deployment/worker: hybernate.io/depends-on: "statefulset/postgres"
```

- An address counts when it names a Service in a scanned namespace, in any form cluster DNS gives it (`postgres`, `postgres.sandbox-42`, `postgres.sandbox-42.svc.cluster.local`, or a headless Service's pod, `postgres-0.postgres-hl`), inside a URL, a `host:port`, or a list of either. The workload is the one the Service's selector picks. A bare word on its own, such as `MODE=postgres`, isn't taken for an address.
- Variable names don't matter, and Service names come from the cluster, not a list the scan keeps.
- Passwords in URLs are shown as `***`, and query strings, which can hold credentials, are dropped.
- **Headless** dependencies are the ones to act on: connections to a headless address go straight to pods, so the doorman can't hold them, and only [`dependsOn`](../concepts/dependencies.md) wakes the dependency first. A dependency reached through a normal Service is woken by the first request to it anyway.
- Secrets are never read, so addresses set there, and ones built in code or read from files, aren't found. The notes say which workloads take variables from Secrets.

**What the numbers mean:**

- Costs are what the workloads' requests cost while running, sidecars included. Without `--cpu-price` and `--memory-price`, they use assumed list prices from AWS on-demand in us-east-1, and the report says so.
- Without history, the scan shows what idle workloads cost and what each hour asleep frees, not a monthly saving: from one moment it can't tell how often a workload would be woken. To measure savings once Hybernate is installed, label workloads `hybernate.io/managed=true` and annotate them `hybernate.io/dry-run=true`: Hybernate measures how often it would have paused them, for how long, and what that would have freed, without ever pausing them. The scan then lists that for each one under **Measured in dry-run**, with the `kubectl hybernate enable` command to start pausing.
- An hour asleep frees capacity; it becomes money when your cluster autoscaler removes it.
- The WHY column gives the evidence, then facts such as a workload not deployed in a week or more.

**Cluster names:** EKS and GKE contexts are shortened to the cluster with its provider and region, such as `staging (EKS us-east-1)` for `arn:aws:eks:us-east-1:123456789012:cluster/staging`. Any other context is shown as named, and two that would shorten to the same name are shown in full. JSON and YAML keep the full context name.

Workloads scaled to zero by something other than Hybernate, and ones labelled `hybernate.io/ignore=true`, aren't counted.

```bash
# Several clusters, with a combined total
kubectl hybernate scan --context staging --context sandboxes
kubectl hybernate scan --all-contexts

# Some namespaces, as JSON or YAML, at your own prices
kubectl hybernate scan -n sandbox-42 -n preview-918 -o json --cpu-price 0.045 --memory-price 0.006

# A month of history from a Prometheus outside the cluster, with a 2h idleAfter
kubectl hybernate scan --window 30d --idle-after 2h --prometheus-url https://thanos.example.com

# CPU right now only
kubectl hybernate scan --window 0
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--context` | | current context | Kubeconfig context to scan; repeat for several |
| `--all-contexts` | | `false` | Scan every context in the kubeconfig |
| `--namespace` | `-n` | all you can read | Namespace to scan; repeat for several |
| `--exclude-namespaces` | | `kube-system`, `kube-public`, `kube-node-lease` | Namespaces to skip |
| `--output` | `-o` | `table` | `table`, `json`, or `yaml` |
| `--limit` | | `25` | Workloads listed per cluster in the table, idle first; `0` for all |
| `--cpu-threshold` | | `10` | CPU use, as a percentage of requests, below which a workload counts as idle |
| `--cpu-price` | | `0.031` | Your price per vCPU-hour, in dollars |
| `--memory-price` | | `0.004` | Your price per GiB-hour of memory, in dollars |
| `--window` | | `7d` | How much Prometheus history to replay, such as `7d` or `36h`; `0` judges from CPU right now only |
| `--idle-after` | | `1h` | How long without activity before the replay pauses a workload, as Hybernate's `idleAfter` |
| `--prometheus-url` | | found in the cluster | Prometheus API to read history from |

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

`wake` marks the ManagedWorkload as active now by setting its `hybernate.io/last-activity` annotation, the same [activity annotation](../concepts/idle-detection.md#activity-annotations) a sandbox UI would set. A paused workload wakes, along with the workloads it [depends on](../concepts/dependencies.md); a running one has its idle clock restarted. It then waits until the workload is Running.

```bash
# Keep it awake for the next two hours, e.g. for a demo
kubectl hybernate wake my-api -n staging --for 2h

# Request the wake and return straight away
kubectl hybernate wake my-api -n staging --wait=false
```

It refuses, and says why, when activity can't wake the workload: `desiredState: Paused` (remove it or set it to `Running`), or a destroyed workload, whose Deployment or StatefulSet is gone.

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--namespace` | `-n` | kubeconfig context's | Namespace of the ManagedWorkload |
| `--for` | | | Also keep the workload awake for this long, by setting `hybernate.io/active-until` |
| `--wait` | | `true` | Wait until the workload is Running |
| `--timeout` | | `5m` | How long to wait |

Your user needs `get` and `patch` on `managedworkloads` in the namespace.

## Enable a Workload

A workload opted in with `hybernate.io/dry-run: "true"` is measured but never paused. `enable` ends that, so Hybernate starts pausing it while idle:

```bash
kubectl hybernate enable checkout-api -n sandbox-42
kubectl hybernate enable statefulset/postgres -n sandbox-42
kubectl hybernate enable --all -n sandbox-42
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
