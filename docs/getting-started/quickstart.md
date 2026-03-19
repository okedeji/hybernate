# Quickstart

This guide walks you through managing your first workload with Hybernate in under 5 minutes.

## 1. Deploy a Sample Workload

If you don't already have a workload to manage, create a simple Deployment:

```bash
kubectl create namespace sandbox

kubectl create deployment my-api \
  --image=nginx:latest \
  --replicas=3 \
  -n sandbox
```

Wait for the pods to be ready:

```bash
kubectl rollout status deployment/my-api -n sandbox
```

## 2. Create a WorkloadPolicy

Apply a WorkloadPolicy to auto-discover and manage workloads in the namespace:

```yaml title="workloadpolicy.yaml" linenums="1"
apiVersion: hybernate.io/v1alpha1
kind: WorkloadPolicy
metadata:
  name: sandbox-policy
  namespace: sandbox
spec:
  mode: auto-manage
  scanInterval: 10m
  idleThreshold: 50
  dryRun: true
```

```bash
kubectl apply -f workloadpolicy.yaml
```

The policy scans the namespace, classifies each workload as Active, Idle, or Wasteful, and auto-creates a ManagedWorkload for each one with sensible defaults.

??? tip "Three ways to manage workloads"

    - **WorkloadPolicy with `auto-manage`** (this quickstart): scans the namespace and auto-creates ManagedWorkloads for discovered workloads. Best for getting started quickly.
    - **WorkloadPolicy with `suggest` + `kubectl hybernate export`**: scans and classifies workloads but doesn't create anything. You review the results and export the ones you want as ManagedWorkload manifests for GitOps.
    - **ManagedWorkload directly**: create a ManagedWorkload CR yourself with full control over every field. Best when you know exactly what you want.

## 3. Check What Was Discovered

```bash
kubectl get workloadpolicy sandbox-policy -n sandbox
```

You should see your workload classified:

```
NAME             MODE          DISCOVERED   ACTIVE   IDLE   WASTEFUL
sandbox-policy   auto-manage   1            0        1      0
```

Check the auto-created ManagedWorkload:

```bash
kubectl get managedworkloads -n sandbox
```

View its status:

```bash
kubectl get managedworkload my-api -n sandbox -o yaml
```

Look at the `status` section:

```yaml title="status" linenums="1"
status:
  phase: Running
  conditions:
    - type: Ready
      status: "True"
```

View events on the resource:

```bash
kubectl describe managedworkload my-api -n sandbox
```

At this point, Hybernate is already working. The forecast engine progresses through phases independently, regardless of `dryRun`:

1. **Observing** — collecting data, no decisions yet. The engine needs at least 24 hours of data before it starts making predictions.
2. **Suggesting** — the engine has enough data to predict daily patterns and starts evaluating idle and scale policies, but only logs what it would do. This is always dry run, even if `dryRun: false`.
3. **Active** — the engine's confidence has crossed the threshold (default 85%). If `dryRun: false`, it now takes real action: pausing, scaling, or destroying workloads. If `dryRun: true`, it continues to log decisions without acting.

You can track which phase the engine is in:

```bash
kubectl get managedworkload my-api -n sandbox -o jsonpath='{.status.prediction}'
```

Since `dryRun` is enabled and the engine starts in Observing, nothing will be touched. You can follow the events to watch it progress:

```bash
kubectl describe managedworkload my-api -n sandbox
```

To see what happens when Hybernate actually takes action, you can bypass the automation and manually trigger a pause.

## 4. Manually Pause the Workload

Set the desired state to override automation and force a pause:

```bash
kubectl patch managedworkload my-api -n sandbox \
  --type merge -p '{"spec":{"desiredState":"Paused"}}'
```

Hybernate will:

1. Capture the current replica count (3)
2. Scale the Deployment to 0
3. Set the phase to `Paused`

Verify:

```bash
kubectl get deployment my-api -n sandbox
# READY: 0/0

kubectl get managedworkload my-api -n sandbox -o jsonpath='{.status.phase}'
# Paused
```

## 5. Resume the Workload

```bash
kubectl patch managedworkload my-api -n sandbox \
  --type merge -p '{"spec":{"desiredState":"Running"}}'
```

Hybernate restores the Deployment to 3 replicas and waits for readiness.

## 6. Enable Automation

Once you're comfortable with what you see in dry run, disable it to let Hybernate act:

```bash
kubectl patch managedworkload my-api -n sandbox \
  --type json -p '[
    {"op": "remove", "path": "/spec/desiredState"},
    {"op": "replace", "path": "/spec/dryRun", "value": false}
  ]'
```

Hybernate will now:

- Monitor CPU usage against the 50m threshold
- Wait for all signals to confirm idle
- Apply the grace period
- Check the forecast engine before acting
- Pause the workload if everything agrees
- Auto-resume when demand returns

## What's Next?

- [ManagedWorkload Guide](../guides/managed-workload.md): full spec reference with examples
- [Idle Detection](../concepts/idle-detection.md): how signals and grace periods work
- [Prometheus Signals](../guides/prometheus-signals.md): add custom PromQL checks
- [WorkloadPolicy](../guides/workload-policy.md): discovery, classification, and auto-manage
- [GitOps Export](../guides/gitops-export.md): export discovered workloads for ArgoCD/Flux
