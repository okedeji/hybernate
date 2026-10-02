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
  cpuIdleThreshold: 10
  memoryIdleThreshold: 10
  dryRun: true
```

```bash
kubectl apply -f workloadpolicy.yaml
```

The policy scans the namespace, classifies each workload as Active or Idle, and auto-creates a ManagedWorkload for each one with sensible defaults.

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
NAME             MODE          DISCOVERED   ACTIVE   IDLE
sandbox-policy   auto-manage   1            0        1
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

At this point, Hybernate is already working. It records when the workload was last active, and once there has been no activity for `idleAfter` (default 1 hour), it pauses the workload. There's no learning period.

You can see the activity clock:

```bash
kubectl get managedworkload my-api -n sandbox -o jsonpath='{.status.activity}'
```

Since `dryRun` is enabled, nothing will be touched: when the clock runs out, the phase becomes `Idle` and a "would pause" event is emitted. You can follow the events to watch it progress:

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

- Record activity: CPU above the threshold, deploys, and activity annotations
- Pause the workload once there has been no activity for `idleAfter`
- Hold off if a confident forecast expects demand within the hour
- Wake it when a request reaches its Service, holding the request until it's Ready
- Wake it when an activity annotation is set, or ahead of forecast demand with `autoResume`

## What's Next?

- [ManagedWorkload Guide](../guides/managed-workload.md): full spec reference with examples
- [Idle Detection](../concepts/idle-detection.md): how the activity clock works
- [WorkloadPolicy](../guides/workload-policy.md): discovery, classification, and auto-manage
- [GitOps Export](../guides/gitops-export.md): export discovered workloads for ArgoCD/Flux
