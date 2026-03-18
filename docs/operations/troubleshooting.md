# Troubleshooting

## Common Issues

### Workload is idle but not being paused

**Check 1: Is dry run enabled?**

```bash
kubectl get managedworkload my-api -n staging -o jsonpath='{.spec.dryRun}'
```

If `true`, the operator evaluates but doesn't act. Set to `false` to enable.

**Check 2: Are all signals confirming?**

```bash
kubectl describe managedworkload my-api -n staging
```

Look at events for signal evaluation results. If any signal denies, idle detection resets.

**Check 3: Is the grace period still running?**

Check the idle signal metric:

```bash
# 3 = InGracePeriod, 4 = Idle
kubectl get --raw /metrics | grep hybernate_idle_signal_result
```

**Check 4: Is the prediction engine active?**

```bash
kubectl get managedworkload my-api -n staging -o jsonpath='{.status.prediction}'
```

If `dailyPhase` is `Observing`, the engine hasn't collected enough data yet (needs 24+ hours).

### Workload keeps cycling between paused and running

This usually means idle detection triggers pause, then auto-resume immediately detects "not idle" (because paused workloads have zero CPU — which is below threshold but the workload has no pods to measure).

**Fix:** Ensure your Prometheus signals check for actual traffic, not just CPU:

```yaml
idlePolicy:
  signals:
    - source: prometheus
      promQL: 'rate(http_requests_total{service="my-api"}[10m]) == 0'
```

### Prediction confidence stays at 0

The confidence scorer needs a full 24-hour window of data before reporting. Wait 24+ hours after creating the ManagedWorkload.

Also check that metrics-server is running and returning data:

```bash
kubectl top pods -n staging
```

### Scale-down is blocked

```bash
kubectl describe managedworkload my-api -n staging
```

Look for:

- **"in stabilization window"** — cooldown from a recent scale event. Wait for the stabilization period to elapse.
- **Guard probe denial** — a Prometheus guard query returned zero/empty. Check the query against Prometheus directly.

### Target not found

```bash
kubectl get managedworkload my-api -n staging -o jsonpath='{.status.conditions}'
```

If you see a `Degraded` condition with "target not found":

- Verify the target exists: `kubectl get deployment my-api -n staging`
- Check that `target.kind` matches (Deployment vs StatefulSet)
- Ensure the ManagedWorkload is in the same namespace as the target

### Duplicate target error

Only one ManagedWorkload can manage a given workload. Check for duplicates:

```bash
kubectl get managedworkloads -n staging -o jsonpath='{range .items[*]}{.metadata.name}: {.spec.target.name}{"\n"}{end}'
```

### Cost data shows $0.00

- Verify `costTracking.enabled: true` is set
- Cost accumulation requires metrics-server data. Check `kubectl top pods`.
- On day 1 of the month, `estimatedMonthlyCost` shows "pending" until day 2.

## Debug Checklist

1. **Operator running?** `kubectl get pods -n hybernate-system`
2. **CRDs installed?** `kubectl get crd managedworkloads.hybernate.io`
3. **Metrics server running?** `kubectl top nodes`
4. **RBAC correct?** `kubectl auth can-i get pods --as=system:serviceaccount:hybernate-system:hybernate-controller-manager`
5. **Events?** `kubectl describe managedworkload <name> -n <ns>`
6. **Logs?** `kubectl logs -n hybernate-system deployment/hybernate-controller-manager`
7. **Status?** `kubectl get managedworkload <name> -n <ns> -o yaml`

## Getting Help

If you've gone through this checklist and still have issues:

1. Check the [GitHub Issues](https://github.com/okedeji/hybernate/issues) for known problems
2. Open a new issue with:
   - Operator version and Kubernetes version
   - ManagedWorkload YAML (sanitized)
   - Relevant operator logs
   - Output of `kubectl describe managedworkload`
