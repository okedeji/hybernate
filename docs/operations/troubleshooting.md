# Troubleshooting

## Common Issues

### Workload is idle but not being paused

**Check 1: Is dry run enabled?**

```bash
kubectl get managedworkload my-api -n staging -o jsonpath='{.spec.dryRun}'
```

If `true`, the operator evaluates but doesn't act. Set to `false` to enable.

**Check 2: When was it last active?**

```bash
kubectl get managedworkload my-api -n staging -o jsonpath='{.status.activity}'
```

`lastActivitySource` says what last kept it awake, and `pauseAt` is when it will pause if nothing else happens. If `pauseAt` keeps moving forward, something is still counting as activity:

- **`cpu`**: usage is above `activity.cpuThreshold`% of requests. Background work (polling, cron loops) can keep a workload busy; raise the threshold if that's expected.
- **`annotation`**: a `hybernate.io/last-activity` annotation is being refreshed, or `hybernate.io/active-until` is in the future. Check both the ManagedWorkload and its target.
- **`rollout`**: the pod template changed recently.

**Check 3: Can Hybernate read CPU?**

```bash
kubectl get managedworkload my-api -n staging \
  -o jsonpath='{.status.conditions[?(@.type=="MetricsAvailable")]}'
```

Without CPU data a busy workload looks idle, so Hybernate doesn't act. `MetricsAvailable=False` says why:

- **`NoPodMetrics`**: the target has replicas, but the Metrics API reports no pods for it. Check that metrics-server is installed (`kubectl top pods -n staging`) and that the target's pods are running.
- **`NoCPURequests`**: the target's containers set no CPU requests, so utilization can't be measured. Set CPU requests.
- **`MetricsUnavailable`**: the Metrics API itself failed. The condition message has the error; a missing `metrics.k8s.io` API means metrics-server isn't installed.

A target scaled to zero replicas is not an error: it's recorded as zero usage.

**Check 4: Is the forecast holding it awake?**

Look for an `IdleVetoed` event in `kubectl describe managedworkload my-api -n staging`. A confident forecast that expects demand in the next hour defers the pause.

**Check 5: Is something depending on it?**

```bash
kubectl get managedworkload postgres -n staging \
  -o jsonpath='{.status.conditions[?(@.type=="HeldByDependents")]}'
```

`HeldByDependents=True` names the awake workloads that list this one in `dependsOn`. It pauses once they have. `DependencyCycle=True` means two workloads depend on each other; remove the cycle. See [Dependencies](../concepts/dependencies.md).

### Workload stuck in Resuming

`WaitingForDependencies=True` means a `waitForReady` dependency isn't Ready yet; the condition names it and how many replicas are ready. Check that dependency's pods. Otherwise, the workload's own pods aren't becoming Ready: resume completes only when every replica is Ready.

### Requests to a paused workload fail instead of waking it

```bash
kubectl get managedworkload my-api -n staging \
  -o jsonpath='{.status.conditions[?(@.type=="WakeOnRequest")]}'
```

- **No condition, or `NotPaused`**: the workload isn't routed. A workload paused with `desiredState: Paused`, or with `wake.onRequest: false`, doesn't wake on request.
- **`NoServices`**: no Service with a selector and a ClusterIP selects the workload's pods. Headless Services aren't routed; see [Wake on Request](../concepts/wake-on-request.md#when-a-workload-isnt-routed).
- **`UnsupportedLoadBalancer`**, or a Service named in the message as not routed: the Service is behind GKE container-native load balancing, which can't use the doorman. See [Compatibility](../reference/compatibility.md#cloud-load-balancers).
- **`DoormanUnavailable`**: no doorman pod is Ready. Check `kubectl get pods -n hybernate-system -l control-plane=doorman`.

If the condition is `DoormanRouted` but requests still fail:

- **The request is closed after a while with no response**: the workload didn't become Ready within `wake.maxWait`. A `RequestNotServed` warning event says so, and `hybernate_doorman_wakes_total{result="timeout"}` counts these. Raise `maxWait`, or check why the pods are slow to become Ready.
- **A 502 from ingress-nginx right after the workload paused**: nginx hadn't picked up the change yet and sent the request to the removed pod. A retry a second later is held and wakes the workload.
- **A 504 from an Ingress after 60 seconds**: the ingress controller gave up first. Raise its upstream timeout, for ingress-nginx `nginx.ingress.kubernetes.io/proxy-read-timeout`.
- **The request is refused right after the workload paused**: the Service's backends hadn't switched to the doorman yet, which takes a second or two. A retry is held and wakes the workload.
- **The request is refused or times out right away, every time**: a NetworkPolicy may block traffic to the doorman (ports 20000-29999) or from it to the workload's pods. See [Network policies](../concepts/wake-on-request.md#network-policies).

The doorman's logs name the workload and Service for each held connection: `kubectl logs -n hybernate-system -l control-plane=doorman`.

### Workload keeps cycling between paused and running

Something keeps waking it. Check the events for `WokeByActivity` (an activity annotation newer than the pause, or a held request: `WokenByRequest` names the Service) or `AutoResume` (the forecast expected demand). A health check or monitor that calls the Service on a timer wakes the workload each time; point it elsewhere or opt the workload out with `wake.onRequest: false`. A tool that refreshes `hybernate.io/last-activity` on a timer, rather than on real use, will keep waking the workload.

### Prediction confidence stays at 0

The confidence scorer needs a full 24-hour window of data before reporting. Wait 24+ hours after creating the ManagedWorkload.

Also check that metrics-server is running and returning data:

```bash
kubectl top pods -n staging
```

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
