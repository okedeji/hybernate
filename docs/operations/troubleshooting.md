# Troubleshooting

## Common Issues

### Workload is idle but not being paused

**Check 1: Is dry run enabled?**

```bash
kubectl get managedworkload my-api -n staging -o jsonpath='{.spec.dryRun}'
```

If `true`, the operator evaluates but doesn't act. To end it, run `kubectl hybernate enable my-api -n staging`, which does the right thing however the workload was set up. On a ManagedWorkload created from the `hybernate.io/managed` label, `spec.dryRun` follows the `hybernate.io/dry-run` annotation on the workload, its namespace, or the cluster default (`defaults.dryRun`), so setting it on the ManagedWorkload is overwritten: set the annotation to `"false"` instead. A value that can't be read, such as `"False"`, counts as dry-run, with an `InvalidSetting` event on the workload. Only on a ManagedWorkload you wrote yourself do you set `spec.dryRun: false`.

**Check 2: When was it last active?**

```bash
kubectl get managedworkload my-api -n staging -o jsonpath='{.status.activity}'
```

`lastActivitySource` says what last kept it awake, and `pauseAt` is when it will pause if nothing else happens. If `pauseAt` keeps moving forward, something is still counting as activity:

- **`cpu`**: usage is above `activity.cpuThreshold`% of requests. Background work (polling, cron loops) can keep a workload busy; raise the threshold if that's expected.
- **`annotation`**: a `hybernate.io/last-activity` annotation is being refreshed, or `hybernate.io/active-until` is in the future. Check both the ManagedWorkload and its target.
- **`rollout`**: the pod template changed recently.
- **`prometheus`**: one of `activity.prometheus` returns a value above zero.
- **`unobserved`**: the operator wasn't watching for a while, such as during a restart, so the clock restarted then.

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

A confident forecast that expects demand in the next hour defers the pause. While it does, the `IdleVetoed` condition is `True`, and its message says when the forecast expects demand; an `IdleVetoed` event marks when the veto began.

```bash
kubectl get managedworkload my-api -n staging \
  -o jsonpath='{.status.conditions[?(@.type=="IdleVetoed")]}'
```

**Check 5: Is anything else holding it?**

`kubectl hybernate status` gives the reason in its NEXT column. Otherwise:

- `desiredState: Running` keeps it running (`ManualOverride=True`), and without an `idlePolicy` nothing pauses it.
- `PrometheusAvailable=False`: a Prometheus activity query fails, or the operator has no `--prometheus-url`.
- `Protected=True`: its namespace is protected.
- `GitOpsConflict=True`: a GitOps tool undid its last pause, and Hybernate waits an hour before pausing again.

**Check 6: Is something depending on it?**

```bash
kubectl get managedworkload postgres -n staging \
  -o jsonpath='{.status.conditions[?(@.type=="HeldByDependents")]}'
```

`HeldByDependents=True` names the awake workloads that depend on this one, declared in `dependsOn` or [learned](../concepts/dependencies.md#learned-dependencies). It pauses once they have; `kubectl hybernate deps postgres -n staging` shows each link and where it came from. `DependencyCycle=True` means two workloads depend on each other; remove the cycle. See [Dependencies](../concepts/dependencies.md).

### Workload stuck in Resuming

`WaitingForDependencies=True` means a `waitForReady` dependency isn't Ready yet; the condition names it and how many replicas are ready. Check that dependency's pods. Otherwise, the workload's own pods aren't becoming Ready: resume completes only when every replica it was restored to is Ready. The `HybernateWorkloadStuck` alert fires after 15 minutes, and `kubectl hybernate status` lists it under `Stuck` after 10.

### Waking a workload by hand

```bash
kubectl hybernate wake my-api -n staging
```

It waits until the workload is Running, or tells you why it can't wake it. See the [kubectl plugin](../getting-started/kubectl-plugin.md#wake-a-workload).

### Requests to a paused workload fail instead of waking it

```bash
kubectl get managedworkload my-api -n staging \
  -o jsonpath='{.status.conditions[?(@.type=="WakeOnRequest")]}'
```

- **No condition, or `NotPaused`**: the workload isn't routed. A workload paused with `desiredState: Paused`, or with `wake.onRequest: false`, doesn't wake on request.
- **`NoServices`**: no Service with a selector and a ClusterIP selects the workload's pods on a TCP port, or every such port is in `hybernate.io/doorman-ignore-ports`. Headless Services aren't routed; see [Wake on Request](../concepts/wake-on-request.md#when-a-workload-isnt-routed).
- **`ServedByOtherPods`**: another workload's Ready pods are behind the same Service, so requests go to them. See [Services shared with other workloads](../concepts/wake-on-request.md#services-shared-with-other-workloads).
- **`UnsupportedLoadBalancer`**, or a Service named in the message as not routed: the Service is behind GKE container-native load balancing, which can't use the doorman. See [Compatibility](../reference/compatibility.md#cloud-load-balancers).
- **`UnsupportedIPFamily`**: no doorman pod has an address in the Service's IP family, such as an IPv6-only Service in a cluster whose doorman pods have only IPv4 addresses.
- **`DoormanUnavailable`**: no doorman pod is Ready. Check `kubectl get pods -A -l control-plane=doorman`.
- **`NoFreePort`**: every doorman port (20000-29999) is in use.
- **`RoutingFailed`**: writing the doorman's EndpointSlices failed; the message has the error, and it's retried with backoff. Check the operator's RBAC and logs.
- **`DoormanDisabled`**: the doorman isn't installed (`doorman.enabled: false`).
- **`TargetNotFound`**: the Deployment or StatefulSet is gone, so there's nothing to wake.

If the condition is `DoormanRouted` but requests still fail:

- **The request is closed after a while with no response**: the workload didn't become Ready within `wake.maxWait`, or its pods couldn't be reached. A `RequestNotServed` warning event says which, and `hybernate_doorman_wakes_total{result=~"timeout|error"}` counts these. Raise `maxWait`, or check why the pods are slow to become Ready.
- **The connection is closed at once, with nothing sent**: a [limit](../concepts/wake-on-request.md#limits) on held connections or wakes was hit; `hybernate_doorman_wakes_total{result="limited"}` counts these.
- **A `503` with no body**: the caller's `User-Agent` looks like a health check or scraper, such as `kube-probe/` or `Prometheus/`, which the doorman never lets wake a workload. It answers them this way only while none of the workload's pods is Ready.
- **A 502 from ingress-nginx right after the workload paused**: nginx hadn't picked up the change yet and sent the request to the removed pod. A retry a second later is held and wakes the workload.
- **A 504 from an Ingress after 60 seconds**: the ingress controller gave up first. Raise its upstream timeout, for ingress-nginx `nginx.ingress.kubernetes.io/proxy-read-timeout`.
- **The request is refused right after the workload paused**: the Service's backends hadn't switched to the doorman yet, which takes a second or two. A retry is held and wakes the workload.
- **The request is refused or times out right away, every time**: a NetworkPolicy may block traffic to the doorman (ports 20000-29999) or from it to the workload's pods. See [Network policies](../concepts/wake-on-request.md#network-policies).

The doorman's logs name the workload and Service for each held connection: `kubectl logs -n hybernate-system -l control-plane=doorman` (use the namespace you installed into).

### Workload keeps cycling between paused and running

Something keeps waking it. Check the events for `WokeByActivity` (an activity annotation newer than the pause, or a held request: `WokenByRequest` names the Service) or `AutoResume` (the forecast expected demand). A health check or monitor that calls the Service on a timer and sends a request wakes the workload each time. The doorman already ignores connections that close without sending anything, and HTTP requests from common probes and scrapers (see [Wake on Request](../concepts/wake-on-request.md#what-wakes-a-workload-and-what-doesnt)); for anything else, point it elsewhere, list its port in `hybernate.io/doorman-ignore-ports` on the Service, or opt the workload out with `wake.onRequest: false`. A tool that refreshes `hybernate.io/last-activity` on a timer, rather than on real use, will keep waking the workload.

`ScaledUp` means something scaled it up outside Hybernate, named in the event and `status.lastScaledUp`. If it's Argo CD or Flux, the workload also reports `GitOpsConflict=True`: it sets the replicas from Git and will undo every pause until it's told to leave them; see [Argo CD and Flux](../guides/gitops.md).

### Prediction confidence stays at 0

The confidence scorer needs a full 24-hour window of observed hours before reporting daily confidence, and 168 for weekly. An hour isn't observed while the operator is down, while CPU can't be read, or while the workload is paused without the doorman, so a workload paused every night without wake on request never gets a full day. See [Forecasting](../concepts/forecasting.md#phase-lifecycle).

Also check that metrics-server is running and returning data:

```bash
kubectl top pods -n staging
```

### Target not found

```bash
kubectl get managedworkload my-api -n staging -o jsonpath='{.status.conditions}'
```

If `TargetAvailable` is `False` with reason `TargetNotFound` (a `TargetNotFound` warning event says so too):

- Verify the target exists: `kubectl get deployment my-api -n staging`
- Check that `target.kind` matches (Deployment vs StatefulSet)
- Ensure the ManagedWorkload is in the same namespace as the target

`spec.target` can't be changed. To manage another workload, create another ManagedWorkload. Reason `TargetIgnored` means the target is labelled `hybernate.io/ignore`.

### Duplicate target error

Only one ManagedWorkload can manage a given workload: the oldest wins, and the others report `DuplicateTarget=True` naming it, and do nothing. Check for duplicates:

```bash
kubectl get managedworkloads -n staging -o jsonpath='{range .items[*]}{.metadata.name}: {.spec.target.name}{"\n"}{end}'
```

### Cost data shows $0.00

- Cost is priced on what the workload's pods request, read from its pods. A workload whose containers set no requests costs nothing.
- `projectedMonthlyCost` and `projectedMonthlySavings` show `pending` until 24 hours of the month have been tracked, including on the first day of every month.
- `savedThisMonth` stays `$0.00` until Hybernate has paused the workload this month: dry-run workloads record what they would have freed in `status.dryRun` instead.
- Totals start again from zero each calendar month, in UTC. See [Cost Tracking](../concepts/cost-tracking.md).

## Debug Checklist

1. **Operator running?** `kubectl get pods -n hybernate-system` (or the namespace you installed into)
2. **CRDs installed?** `kubectl get crd managedworkloads.hybernate.io`
3. **Metrics server running?** `kubectl top nodes`
4. **RBAC correct?** `kubectl auth can-i get pods --as=system:serviceaccount:hybernate-system:hybernate` (the ServiceAccount is `hybernate` for a Helm release named `hybernate`, and `hybernate-controller-manager` with kustomize)
5. **Events?** `kubectl describe managedworkload <name> -n <ns>`
6. **Logs?** `kubectl logs -n hybernate-system -l control-plane=controller-manager`
7. **Status?** `kubectl get managedworkload <name> -n <ns> -o yaml`

## Getting Help

If you've gone through this checklist and still have issues:

1. Check the [GitHub Issues](https://github.com/okedeji/hybernate/issues) for known problems
2. Open a new issue with:
   - Operator version (`kubectl get deploy -n hybernate-system -o wide` shows the image), plugin version (`kubectl hybernate version`), and Kubernetes version
   - ManagedWorkload YAML (sanitized)
   - Relevant operator logs
   - Output of `kubectl describe managedworkload`
