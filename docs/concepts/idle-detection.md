# Idle Detection

Hybernate keeps an **activity clock** for every ManagedWorkload: the last time anything showed the workload was in use. When the clock is older than `idlePolicy.idleAfter`, the workload is paused: scaled to zero, keeping its replica count and a resource snapshot. Any single sign of activity resets it.

There's no learning period. A new workload can be paused as soon as it has gone `idleAfter` without activity.

```yaml title="managedworkload.yaml" linenums="1"
spec:
  idlePolicy:
    idleAfter: 1h        # default
    activity:
      cpuThreshold: 10   # percent of CPU requests; default
```

## What counts as activity

Every source is checked, and the most recent one wins. None has priority over another.

| Source | Counts as activity when | Setup |
|--------|------------------------|-------|
| **CPU** | Usage is above `activity.cpuThreshold`% of the CPU requested across all replicas | Requires metrics-server |
| **Deploy** | The target's pod template changes: a new image, environment variable, or other template edit | None |
| **`hybernate.io/last-activity`** | The annotation holds a time newer than the last recorded activity | Set by your own tooling |
| **`hybernate.io/active-until`** | The annotation holds a time in the future (a hold, not an activity time) | Set by your own tooling |
| **`hybernate.io/last-request`** | The annotation holds a time newer than the last recorded activity | Set by the [doorman](wake-on-request.md) when it holds a request for the paused workload |
| **Prometheus** | Any query in `activity.prometheus` returns a value above zero | `--prometheus-url`; see the [Prometheus Activity Guide](../guides/prometheus-signals.md) |

Changes to the replica count don't count as a deploy, so Hybernate's own pause and resume never look like activity.

Memory isn't a source: it stays allocated whether or not anyone uses the workload.

CPU is measured for the containers in the target's pod template, including native sidecars (init containers with `restartPolicy: Always`). Sidecars injected when the pod is created, such as an Istio or Linkerd proxy, aren't counted: their own background work isn't use of the workload, and they aren't in the template's requests. [Cost](cost-tracking.md) does count them, since they cost the same as any container and pausing frees them too.

### Activity annotations

Set either annotation on the ManagedWorkload or on its target Deployment or StatefulSet. Values are RFC 3339 times.

```bash
# Record activity now, e.g. from a developer portal when someone opens an environment
kubectl annotate deployment my-api -n staging --overwrite \
  hybernate.io/last-activity="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# Keep the workload awake until a time, e.g. during a demo
kubectl annotate deployment my-api -n staging --overwrite \
  hybernate.io/active-until="2026-10-03T18:00:00Z"
```

From a terminal, [`kubectl hybernate wake`](../getting-started/kubectl-plugin.md#wake-a-workload) sets them for you and waits until the workload is Running.

- A `last-activity` time in the future is treated as now. Use `active-until` for a deliberate hold, since it says when the hold ends.
- Annotations can only keep a workload awake or wake it. They never cause a pause.
- Malformed values are ignored and logged.

## The decision

A workload at zero replicas that Hybernate didn't pause isn't checked at all: it's already off, and [left that way](../guides/pause.md#workloads-already-at-zero). For any other, each check runs in this order:

1. CPU, or a configured Prometheus query, can't be read: stay awake (see [below](#when-a-source-cant-be-read)).
2. `active-until` is in the future: stay awake.
3. The last activity is less than `idleAfter` ago: stay awake.
4. The [forecast](forecasting.md) is confident (`DailyActive` or later) and predicts demand above `cpuThreshold` in the hour under way or the next: stay awake. The `IdleVetoed` condition is `True`, with reason `ForecastExpectsDemand` and a message naming the hour, while this holds the pause back, and an `IdleVetoed` event marks the start of each veto.
5. Another workload that [depends on](dependencies.md) this one is awake, or `dependsOn` forms a cycle: stay awake (`HeldByDependents` or `DependencyCycle`).
6. Otherwise the clock has run out. The phase becomes `Idle`, with an `IdleDetected` event, and the workload is [paused](../guides/pause.md).

Without an `idlePolicy`, none of this runs, and the workload is only paused when someone asks.

A [pause request](../guides/pause.md#pause-now), from `kubectl hybernate pause`, runs the clock out at once. Steps 1, 3 and 4 don't apply, and nor does the hour Hybernate waits after a GitOps tool undid its last pause, since someone asked; an `active-until` hold, awake dependents and a `dependsOn` cycle still keep the workload up, and the request is answered with the reason.

With `dryRun: true`, the phase still becomes `Idle` and the `IdleDetected` event carries a `[dry-run]` prefix, but nothing is changed. If activity resumes, the phase goes back to `Running`. See [Dry Run](../guides/dry-run.md).

The forecast never blocks a pause until it's confident, so a workload with no history is handled by the clock alone.

## When a source can't be read

Hybernate never pauses a workload while one of its activity sources can't be read: without that data a busy workload looks idle. For CPU, the `MetricsAvailable` condition says why:

| Reason | Meaning |
|--------|---------|
| `NoPodMetrics` | The target has replicas but the Metrics API reports no pods. Check that metrics-server is installed. |
| `NoCPURequests` | The target's containers set no CPU requests, so utilization can't be measured. Set CPU requests. |
| `MetricsUnavailable` | The Metrics API itself failed. The condition message has the error. |

A target scaled to zero isn't an error: it has no usage, which is real data.

For Prometheus queries, the `PrometheusAvailable` condition reports `EndpointNotConfigured` or `QueryFailed`.

## Restarts and outages

The clock is stored in `status.activity`, so it survives operator restarts. While a workload is awake it's checked about once a minute; the clock's timestamps are written to status at most every 5 minutes, along with cost, and immediately whenever the phase or a condition changes, or when new activity is more than a tenth of `idleAfter` (at most 5 minutes) ahead of what's written. Activity seen between writes is held in memory, so it still counts, and what's written is never far enough behind for a restart to pause a workload early.

If the operator wasn't running for more than about 7 minutes (two missed checks beyond the write interval), it can't know whether there was activity in the meantime, so the clock restarts from the time it resumes watching (`lastActivitySource: unobserved`). An outage can delay a pause, but never cause one.

```yaml
status:
  activity:
    lastActivityTime: "2026-10-02T09:14:00Z"
    lastActivitySource: cpu    # created, woke, request, cpu, rollout, annotation, prometheus, unobserved, or scaled-up
    pauseAt: "2026-10-02T10:14:00Z"
    lastEvaluatedTime: "2026-10-02T09:41:00Z"
```

## Waking up

A paused workload wakes when:

- A request reaches one of its Services. The request is held while the workload starts, then answered; see [Wake on Request](wake-on-request.md).
- A `hybernate.io/last-activity`, `last-request` or `active-until` annotation on the ManagedWorkload or its target changes after the pause began, whatever time it states, so a clock that's behind the operator's can't lose a wake. Only a change counts: the pause records the annotations as they stood, so a value set before it, even one stating a later time, doesn't wake it. `kubectl hybernate wake` sets one, and this is how a "start environment" button in a developer portal works.
- `autoResume: true` is set and a confident forecast predicts demand above `cpuThreshold` for the current hour, or for the next hour once it's 15 minutes away, so the workload is ready before people arrive.
- Something other than Hybernate scales it up, such as `kubectl scale`.
- Dry-run is turned on: dry-run never leaves a workload paused.

Each wake restarts the clock, so a woken workload gets a full `idleAfter` before it can pause again. The clock records `lastActivitySource: request` when a held request woke it, `scaled-up` when something else scaled it up, and `woke` for any other wake.
