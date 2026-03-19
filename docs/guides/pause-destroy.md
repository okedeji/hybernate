# Pause & Destroy Guide

Hybernate provides two lifecycle actions beyond scaling: **pause** (scale to zero) and **destroy** (delete the workload). Both include safety mechanisms for data preservation.

## Pause

Pausing scales the workload to zero replicas. The Deployment or StatefulSet still exists; only the pods are removed.

### What Happens When a Workload Is Paused

1. The current replica count is saved to `status.pause.previousReplicas`
2. A resource snapshot is captured (CPU, memory, storage per replica) for cost savings calculation
3. The workload is scaled to 0
4. The phase transitions to `Paused`
5. `status.pause.pausedAt` is set

### Triggering a Pause

**Manually:**

```bash
kubectl patch managedworkload my-api -n staging \
  --type merge -p '{"spec":{"desiredState":"Paused"}}'
```

**Automatically:** When idle detection confirms idle and `idlePolicy.action` is `pause` or `auto`.

### Pause Expiry

Configure what happens if a workload stays paused too long:

```yaml title="managedworkload.yaml" linenums="1"
spec:
  pause:
    expireAfter: "24h"
    expireAction: Resume
```

| `expireAction` | What happens |
|---------------|-------------|
| `resume` | Workload is scaled back to its previous replica count |
| `destroy` | Workload is deleted (with optional PVC retention) |

If `expireAfter` is not set, the workload stays paused indefinitely.

### Resume

Resuming restores the saved replica count and waits for pods to become ready.

**Manually:**

```bash
kubectl patch managedworkload my-api -n staging \
  --type merge -p '{"spec":{"desiredState":"Running"}}'
```

**Automatically:**

- When `expireAfter` elapses with `expireAction: Resume`
- When `autoResume: true` is set and signals no longer confirm idle

## Destroy

Destroying deletes the target Deployment or StatefulSet. The ManagedWorkload CR remains to track PVC retention and cost savings.

### What Happens When a Workload Is Destroyed

1. A resource snapshot is captured for cost savings calculation
2. The target Deployment/StatefulSet is deleted
3. The phase transitions to `Destroyed`
4. `status.destroy.destroyedAt` is set
5. If PVC retention is configured, `status.destroy.pvcRetentionExpiresAt` is set

### Triggering a Destroy

**Manually:**

```bash
kubectl patch managedworkload my-api -n staging \
  --type merge -p '{"spec":{"desiredState":"Destroyed"}}'
```

**Automatically:**

- When `idlePolicy.action` is `destroy` and idle is confirmed
- When pause expires with `expireAction: Destroy`

### PVC Retention

By default, PVCs are not cleaned up when a workload is destroyed. They persist independently. Configure retention to clean them up on a schedule:

```yaml title="managedworkload.yaml" linenums="1"
spec:
  destroy:
    pvcRetention: "168h"       # Keep PVCs for 7 days after destroy
    pvcRetentionWarning: "24h" # Emit warning event 24h before cleanup
```

**Timeline:**

```
Destroyed ────────────────────────► PVC Warning ──► PVC Cleanup
    t=0                              t=144h          t=168h
```

The operator matches PVCs using the workload's pod template selector labels.

!!! warning
    Once PVCs are deleted, the data is gone. Set `pvcRetentionWarning` to give users time to recover data before cleanup.

To cancel a scheduled PVC cleanup, remove `pvcRetention` from the spec. The operator detects the change and clears the expiry timer, preserving PVCs indefinitely.

### PVC Retention Without Destroy

PVC retention only applies when a workload is destroyed. Paused workloads keep their PVCs unconditionally (only pods are removed, the workload object and PVCs remain).

## Cost Savings During Pause and Destroy

Cost tracking is always enabled. During pause and destroy:

- **While paused:** CPU and memory savings accrue every reconcile. Storage savings are zero (PVCs persist).
- **While destroyed:** CPU and memory savings accrue. Storage savings begin accruing after PVC retention expires and PVCs are cleaned up.

## Finalizer

The `hybernate.io/cleanup` finalizer is automatically added to every ManagedWorkload. It ensures that:

- If a ManagedWorkload CR is deleted while PVC retention is pending, the operator runs PVC cleanup before allowing the deletion to complete
- Paused workloads are resumed before the ManagedWorkload CR is removed (if applicable)
