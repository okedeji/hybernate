# Idle Detection

Hybernate keeps an **activity clock** for every ManagedWorkload: the last time anything showed the workload was in use. When the clock is older than `idlePolicy.idleAfter`, the workload is paused (or destroyed). Any single sign of activity resets it.

There's no learning period. A new workload can be paused as soon as it has gone `idleAfter` without activity.

```yaml title="managedworkload.yaml" linenums="1"
spec:
  idlePolicy:
    idleAfter: 1h        # default
    action: pause        # or destroy
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
| **Prometheus** | Any query in `activity.prometheus` returns a value above zero | `--prometheus-url`; see the [Prometheus Activity Guide](../guides/prometheus-signals.md) |

Changes to the replica count don't count as a deploy, so Hybernate's own pause and resume never look like activity.

Memory isn't a source: it stays allocated whether or not anyone uses the workload.

CPU is measured for the containers in the target's pod template, including native sidecars (init containers with `restartPolicy: Always`). Sidecars injected when the pod is created, such as an Istio or Linkerd proxy, aren't counted: their own background work isn't use of the workload, and they aren't in the template's requests. [Cost](cost-tracking.md) does count them, since they cost the same as any container and pausing frees them too.

### Activity annotations

Set either annotation on the ManagedWorkload or on its target Deployment or StatefulSet. Values are RFC 3339 times.

```bash
# Record activity now, e.g. from a sandbox UI when a developer opens an environment
kubectl annotate deployment my-api -n staging --overwrite \
  hybernate.io/last-activity="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# Keep the workload awake until a time, e.g. during a demo
kubectl annotate deployment my-api -n staging --overwrite \
  hybernate.io/active-until="2026-10-03T18:00:00Z"
```

- A `last-activity` time in the future is treated as now. Use `active-until` for a deliberate hold, since it says when the hold ends.
- Annotations can only keep a workload awake or wake it. They never cause a pause.
- Malformed values are ignored and logged.

## The decision

Each check runs in this order:

1. `desiredState` is set: the workload is under manual control, and automation does nothing.
2. `active-until` is in the future: stay awake.
3. The last activity is less than `idleAfter` ago: stay awake.
4. The [forecast](forecasting.md) is confident (`DailyActive` or later) and predicts demand above `cpuThreshold` in the next hour: stay awake.
5. Another workload that [depends on](dependencies.md) this one is awake, or the dependencies form a cycle: stay awake.
6. Otherwise the clock has run out. The phase becomes `Idle` and the idle action runs.

With `dryRun: true`, the phase still becomes `Idle` and a "would pause" event is emitted once, but nothing is changed. If activity resumes, the phase goes back to `Running`.

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

The clock is stored in `status.activity`, so it survives operator restarts. While a workload is awake it's checked about once a minute; the clock's timestamps are written to status at most every 5 minutes, along with cost, and immediately whenever the phase or a condition changes. Activity seen between writes is held in memory, so it still counts.

If the operator wasn't running for more than about 7 minutes (two missed checks beyond the write interval), it can't know whether there was activity in the meantime, so the clock restarts from the time it resumes watching (`lastActivitySource: unobserved`). An outage can delay a pause, but never cause one.

```yaml
status:
  activity:
    lastActivityTime: "2026-10-02T09:14:00Z"
    lastActivitySource: cpu    # created, woke, request, cpu, rollout, annotation, prometheus, or unobserved
    pauseAt: "2026-10-02T10:14:00Z"
    lastEvaluatedTime: "2026-10-02T09:41:00Z"
```

## Waking up

A paused workload wakes when:

- A request reaches one of its Services. The request is held while the workload starts, then answered; see [Wake on Request](wake-on-request.md).
- A `hybernate.io/last-activity` annotation is set to a time after the pause, or an `active-until` hold is in the future. This is how a "start environment" button in a sandbox UI works.
- `autoResume: true` is set and a confident forecast predicts demand above `cpuThreshold` for the current hour, so the workload is ready before people arrive.
- `pause.expireAfter` elapses with `expireAction: resume`, or `desiredState` is set to `Running`.

Each wake restarts the clock, so a woken workload gets a full `idleAfter` before it can pause again. The clock records `lastActivitySource: request` when a held request woke it, and `woke` for any other wake.

## Idle actions

| Action | Behavior |
|--------|----------|
| `pause` | Scales to zero and records the replica count and a resource snapshot. The default. |
| `destroy` | Deletes the workload, with optional PVC retention. |
