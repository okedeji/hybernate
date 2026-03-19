# Dry Run Guide

Dry run mode lets you observe what Hybernate would do without it taking any action. The operator evaluates all policies, emits events, and updates predictions, but never modifies your workloads.

## Enabling Dry Run

### Per Workload

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
    idleThreshold: 50
    gracePeriod: "5m"
  prediction:
    confidence: 85
  dryRun: true  # Observe only
```

### Via WorkloadPolicy (for auto-managed workloads)

```yaml title="workloadpolicy.yaml" linenums="1"
apiVersion: hybernate.io/v1alpha1
kind: WorkloadPolicy
metadata:
  name: staging-policy
  namespace: staging
spec:
  mode: auto-manage
  dryRun: true  # Auto-created ManagedWorkloads inherit this
```

## What Happens in Dry Run

The operator runs its full evaluation pipeline:

| Action | Dry Run Behavior |
|--------|-----------------|
| Idle detection | Signals are checked, grace period is tracked, and events are emitted, but the workload is **not** paused |
| Scaling | Forecast engine proposes targets and constraints are evaluated, but replicas are **not** changed |
| Pause expiry | Expiry is detected, but the workload is **not** resumed or destroyed |
| Cost tracking | Costs are accumulated normally (resource usage is real regardless of management) |
| Prediction engine | Data points are observed and confidence builds normally |
| Events | All events are emitted with a `[DRY RUN]` prefix |
| Status | Phase and conditions update to reflect what *would* happen |

## Observing Dry Run Results

### Events

```bash
kubectl describe managedworkload my-api -n staging
```

Look for events like:

```
[DRY RUN] Idle confirmed — would pause workload (grace period elapsed, all signals confirm)
[DRY RUN] Scale — would scale from 5 to 3 replicas (prediction: low demand)
```

### Status

The status reflects the evaluated state:

```bash
kubectl get managedworkload my-api -n staging -o yaml
```

```yaml title="status" linenums="1"
status:
  phase: Running  # Stays Running because no action was taken
  prediction:
    dailyPhase: DailyActive
    dailyConfidence: 87
    weeklyPhase: Observing
    weeklyConfidence: 0
```

## Recommended Workflow

1. **Deploy with `dryRun: true`**. Observe events and status for a few days.
2. **Check prediction confidence**. Wait until the forecast engine reaches DailyActive and confidence exceeds your threshold.
3. **Review events**. Confirm the operator would have made the right decisions.
4. **Disable dry run**. Flip to `false` to enable automation.

```bash
kubectl patch managedworkload my-api -n staging \
  --type merge -p '{"spec":{"dryRun":false}}'
```

## When to Use Dry Run

- First time deploying a ManagedWorkload
- After changing idle or scale policies
- When onboarding a new namespace via WorkloadPolicy
- Before moving from `suggest` to `auto-manage` mode
- In production environments where you want to validate before acting
