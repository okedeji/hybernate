# Dry Run Guide

Dry run mode lets you observe what Hybernate would do without it ever reducing a workload's availability. The operator evaluates all policies, emits events, and updates predictions, but never pauses a workload. Wakes are never held back.

## Enabling Dry Run

### On a Labelled Workload

```yaml title="deployment.yaml" linenums="1"
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-api
  namespace: staging
  labels:
    hybernate.io/managed: "true"
  annotations:
    hybernate.io/dry-run: "true"
```

### On a Namespace

Every opted-in workload in the namespace is measured, unless it sets its own `hybernate.io/dry-run: "false"`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: staging
  labels:
    hybernate.io/managed: "true"
  annotations:
    hybernate.io/dry-run: "true"
```

### Across the Cluster

Set `defaults.dryRun: true` in the Helm values to measure every opted-in workload that doesn't say otherwise, on the workload or its namespace.

A `hybernate.io/dry-run` value that can't be read, such as `"True"` or `"yes"`, turns dry-run on, with an `InvalidSetting` warning event on the workload: a typo in the setting meant to stop pauses must never start them.

### On a ManagedWorkload You Wrote

```yaml title="managedworkload.yaml" linenums="1"
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata:
  name: my-api
  namespace: staging
spec:
  target:
    kind: Deployment
    name: my-api
  idlePolicy:
    idleAfter: 1h
  prediction:
    confidence: 85
  dryRun: true
```

On a ManagedWorkload created from the label, `spec.dryRun` follows the annotations: setting it there by hand is overwritten.

## What Happens in Dry Run

The operator runs its full evaluation pipeline:

| Action | Dry Run Behavior |
|--------|-----------------|
| Idle detection | The activity clock runs and the phase becomes `Idle` when it runs out, but the workload is **not** paused |
| Forecast veto and dependency holds | Applied and reported as usual: `IdleVetoed`, `HeldByDependents`, `DependencyCycle` |
| `desiredState: Paused` | Not acted on: the `WouldPause` condition is set, with one `[dry-run]` event, and the workload keeps running |
| A workload Hybernate had already paused | Woken as soon as dry-run is turned on, with a `DryRunWake` event: dry-run never leaves a workload paused |
| Wakes | Never held back: `desiredState: Running`, `kubectl hybernate wake`, and activity work as usual |
| Cost tracking | Costs are accumulated normally, all as awake time, since the workload never pauses; see [Cost Tracking](../concepts/cost-tracking.md#in-dry-run) |
| Prediction engine | Data points are observed and confidence builds normally |
| Events | The events for decisions dry-run holds back carry a `[dry-run]` prefix; see [Events](#events) |
| Status | Phase and conditions reflect what *would* happen, and `status.dryRun` sums it up |

## Observing Dry Run Results

### What Dry Run Measured

Hybernate keeps a running summary of the pauses it would have made. A would-be pause starts when the activity clock runs out, and ends at the next activity, which is when a paused workload would have been woken:

```bash
kubectl get managedworkload my-api -n staging -o jsonpath='{.status.dryRun}'
```

```yaml title="status.dryRun"
since: "2026-10-01T09:00:00Z"
pauses: 4
slept: 96h12m0s
freedCPUHours: "192400m"
freedMemoryHours: "384800m"
estimatedSavings: $7.50
```

`freedCPUHours` and `freedMemoryHours` are the vCPU-hours and GiB-hours the workload's replicas requested during `slept`: what the would-be pauses would have freed, kept to a billionth of an hour. `estimatedSavings` is those hours priced at the workload's [cost rates](../concepts/cost-tracking.md#prices), for display; the hours are the record. Like all savings, it becomes money only if your cluster autoscaler removes the capacity freed.

`slept` counts finished would-be pauses. While the phase is `Idle`, the one under way, which began at `status.lastTransitionTime`, is added when it ends; `resources` is what the workload ran when it began.

The summary starts when dry-run does, and is removed when dry-run ends: once Hybernate is pausing, `status.cost` records what it actually frees.

`kubectl hybernate scan` shows the same summary in each dry-run workload's COULD SLEEP, WAKES, and COULD SAVE/MO columns, with a total in the headline and the command to start pausing:

```
  2 workloads are in dry-run: measured by Hybernate since starting, they would have slept 140 hours, freeing $18.20,
  about $134 a month.

  NAMESPACE   WORKLOAD         STATE            BECAUSE                                     COST/MO   COULD SLEEP   WAKES   COULD SAVE/MO
  preview-7   deployment/api   idle (dry-run)   no activity for 5h; measuring since Oct 1   $96       96h           4       $91
```

The scan prices the hours at its own prices, so `--cpu-price` and `--memory-price` apply.

### Events

```bash
kubectl describe managedworkload my-api -n staging
```

When the clock runs out, and when activity ends the would-be pause:

```
[dry-run] my-api: no activity for 1h0m0s, last seen from cpu; pause
[dry-run] my-api: activity resumed (cpu): would have slept 3h12m, freeing $0.25; since Oct 1: would have paused 4 times, slept 96h12m, freeing $7.50
```

The `[dry-run]` prefix marks the decisions dry-run holds back: `IdleDetected`, `ActivityResumed`, `IdleVetoed`, `HeldByDependents`, `DependencyCycle`, and the `Paused` event for a `desiredState: Paused` it doesn't act on. Events about what really happens, such as a wake, a forecast update, or a metrics problem, have no prefix.

### Phase

In dry-run the phase goes `Running` → `Idle` when the clock runs out, and back to `Running` at the next activity. The workload is never scaled down.

## Recommended Workflow

1. **Opt in with `hybernate.io/dry-run: "true"`**. Let it measure for a few days, across a weekend if the workload is used on weekdays.
2. **Review what it measured**. `status.dryRun` or `kubectl hybernate scan` says how often it would have paused, for how long, and what that would have freed; the events say when.
3. **End dry run**:

    ```bash
    kubectl hybernate enable my-api -n staging
    kubectl hybernate enable --all -n staging   # every workload in the namespace
    ```

    For a labelled workload, `enable` sets its own `hybernate.io/dry-run` annotation to `"false"`, which wins over its namespace's and the cluster default. With `--all` it sets the namespace's annotation to `"false"` instead and drops the workloads' own. For a ManagedWorkload you wrote, it sets `spec.dryRun: false`. If Argo CD or Flux applies the object, it changes nothing and prints the change to make in Git instead. See [`kubectl hybernate enable`](../getting-started/kubectl-plugin.md#enable-a-workload).

## When to Use Dry Run

- The first time you opt a workload or namespace in
- After changing its settings
- In environments where you want to validate before acting
