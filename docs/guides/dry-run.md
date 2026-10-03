# Dry Run Guide

Dry run mode lets you observe what Hybernate would do without it taking any action. The operator evaluates all policies, emits events, and updates predictions, but never modifies your workloads.

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
  dryRun: true
```

## What Happens in Dry Run

The operator runs its full evaluation pipeline:

| Action | Dry Run Behavior |
|--------|-----------------|
| Idle detection | The activity clock runs and the phase becomes `Idle` when it runs out, but the workload is **not** paused |
| Pause expiry | Expiry is detected, but the workload is **not** resumed or destroyed |
| Cost tracking | Costs are accumulated normally (resource usage is real regardless of management) |
| Prediction engine | Data points are observed and confidence builds normally |
| Events | All events are emitted with a `[dry-run]` prefix |
| Status | Phase and conditions update to reflect what *would* happen |

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
estimatedSavings: $12.48
```

`estimatedSavings` prices the replicas the workload ran at each would-be pause at its [cost rates](../concepts/cost-tracking.md). Like all savings, it becomes money only if your cluster autoscaler removes the capacity freed. `slept` counts finished would-be pauses; while the phase is `Idle`, the one under way is added when it ends.

The summary starts when dry-run does, and is removed when dry-run ends: once Hybernate is pausing, `status.cost` records what it actually frees.

`kubectl hybernate scan` shows the same summary for every workload in dry-run, with the command to start pausing it:

```
  Measured in dry-run, had Hybernate been pausing them:
  sandbox-7   deployment/api   since Oct 1: would have paused 4 times, slept 96h, freeing $12.48
```

The scan prices the hours at its own prices, so `--cpu-price` and `--memory-price` apply.

### Events

```bash
kubectl describe managedworkload my-api -n staging
```

When the clock runs out, and when activity ends the would-be pause:

```
[dry-run] my-api: no activity for 1h0m0s, last seen from cpu; pause
[dry-run] my-api: activity resumed (cpu): would have slept 3h12m, freeing $0.42; since Oct 1: would have paused 4 times, slept 96h12m, freeing $12.48
```

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

    This removes the annotation, or sets the workload's own to `"false"` when the dry-run comes from its namespace. If Argo CD or Flux applies the workload, it prints the change to make in Git instead. For a ManagedWorkload you wrote, set `spec.dryRun: false`.

## When to Use Dry Run

- The first time you opt a workload or namespace in
- After changing its settings
- In environments where you want to validate before acting
