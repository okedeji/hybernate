# Quickstart

This guide walks you through managing your first workload with Hybernate in under 5 minutes.

## 1. Deploy a Sample Workload

If you don't already have a workload to manage, create a simple Deployment:

```bash
kubectl create namespace staging

kubectl create deployment my-api \
  --image=nginx:latest \
  --replicas=3 \
  -n staging
```

Wait for the pods to be ready:

```bash
kubectl rollout status deployment/my-api -n staging
```

## 2. Create a ManagedWorkload

Apply a ManagedWorkload CR that tells Hybernate to watch your Deployment:

```yaml
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
    cpuThreshold: "50m"
    gracePeriod: "5m"
  pause:
    expireAfter: "1h"
    expireAction: Resume
  costTracking:
    enabled: true
  dryRun: true
```

```bash
kubectl apply -f managedworkload.yaml
```

!!! tip
    Start with `dryRun: true` to observe what Hybernate would do without it taking action. Check the events and status to build confidence, then set it to `false`.

## 3. Check the Status

```bash
kubectl get managedworkload my-api -n staging -o yaml
```

Look at the `status` section:

```yaml
status:
  phase: Running
  conditions:
    - type: Ready
      status: "True"
```

View operator events:

```bash
kubectl describe managedworkload my-api -n staging
```

## 4. Manually Pause the Workload

To see the pause/resume lifecycle in action, set the desired state:

```yaml
spec:
  desiredState: Paused
```

```bash
kubectl patch managedworkload my-api -n staging \
  --type merge -p '{"spec":{"desiredState":"Paused"}}'
```

Hybernate will:

1. Capture the current replica count (3)
2. Scale the Deployment to 0
3. Set the phase to `Paused`

Verify:

```bash
kubectl get deployment my-api -n staging
# READY: 0/0

kubectl get managedworkload my-api -n staging -o jsonpath='{.status.phase}'
# Paused
```

## 5. Resume the Workload

```bash
kubectl patch managedworkload my-api -n staging \
  --type merge -p '{"spec":{"desiredState":"Running"}}'
```

Hybernate restores the Deployment to 3 replicas and waits for readiness.

## 6. Enable Automation

Once you're comfortable, remove `desiredState` and `dryRun` to let Hybernate manage the workload automatically:

```bash
kubectl patch managedworkload my-api -n staging \
  --type json -p '[
    {"op": "remove", "path": "/spec/desiredState"},
    {"op": "replace", "path": "/spec/dryRun", "value": false}
  ]'
```

Hybernate will now:

- Monitor CPU usage against the 50m threshold
- Wait for all signals to confirm idle
- Apply the 5-minute grace period
- Pause the workload if it remains idle
- Auto-resume after 1 hour (per `expireAfter`)

## What's Next?

- [ManagedWorkload Guide](../guides/managed-workload.md) — full spec reference with examples
- [Idle Detection](../concepts/idle-detection.md) — how signals and grace periods work
- [Prometheus Signals](../guides/prometheus-signals.md) — add custom PromQL checks
- [WorkloadPolicy](../guides/workload-policy.md) — auto-discover and classify workloads
