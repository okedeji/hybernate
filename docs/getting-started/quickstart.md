# Quickstart

This guide takes one workload from "idle and costing money" to "paused while idle, woken when used", measuring it first so nothing is paused until you say so.

It assumes Hybernate is [installed](installation.md) and you have the [kubectl plugin](kubectl-plugin.md).

## 1. A sample workload

If you don't have one to try, create a Deployment:

```bash
kubectl create namespace dev
kubectl create deployment my-api --image=nginx:1.27-alpine --replicas=2 -n dev
kubectl set resources deployment my-api -n dev --requests=cpu=100m,memory=64Mi
kubectl expose deployment my-api -n dev --port=80
kubectl rollout status deployment/my-api -n dev
```

## 2. See what's idle

```bash
kubectl hybernate scan -n dev
```

The scan shows each workload, whether it's idle right now and why, and what it costs while running. A workload deployed in the last hour counts as active, as Hybernate would treat it, so a brand-new one shows as active until then.

## 3. Opt it in, measuring first

```bash
kubectl label deployment my-api -n dev hybernate.io/managed=true
kubectl annotate deployment my-api -n dev hybernate.io/dry-run=true hybernate.io/idle-after=5m
```

The label opts the workload in; the annotations are its settings. Here, dry-run means it's measured and never paused, and `idle-after: 5m` makes the example quick (the default is an hour). In real use, these usually go in the workload's manifest or Helm values, or on its namespace to cover everything in it. See [Opting In](../guides/opt-in.md).

Hybernate creates a ManagedWorkload for it:

```bash
kubectl get managedworkloads -n dev
```

```
NAME     PHASE     AGE
my-api   Running   10s
```

## 4. Watch it measure

```bash
kubectl get managedworkload my-api -n dev -o jsonpath='{.status.activity}'
```

`status.activity` is the activity clock: when the workload was last active, from what, and when it would pause. Once there's been no activity for `idle-after`, the phase becomes `Idle` and an event says it would have paused. Nothing is scaled down in dry-run:

```bash
kubectl describe managedworkload my-api -n dev
```

Each would-be pause is added up in `status.dryRun`: how many times it would have paused, how long it would have slept, and what that would have freed. Mark it active to end one, as a developer portal or your own tooling would:

```bash
kubectl annotate deployment my-api -n dev --overwrite hybernate.io/last-activity=$(date -u +%Y-%m-%dT%H:%M:%SZ)
kubectl get managedworkload my-api -n dev -o jsonpath='{.status.dryRun}'
```

`kubectl hybernate scan -n dev` shows the same summary.

Requests to a running workload don't count as activity by themselves: Hybernate sees its CPU, deploys and activity annotations, and requests only through a [Prometheus query](../guides/prometheus-signals.md) you give it, or once it's paused, through the doorman. An nginx serving the odd request uses almost no CPU, so it would pause.

## 5. Let it pause

When you're happy with what you see:

```bash
kubectl hybernate enable my-api -n dev
```

This sets the workload's `hybernate.io/dry-run` annotation to `"false"`. Once the workload has had no activity for `idle-after`, Hybernate scales it to zero, remembering it had 2 replicas:

```bash
kubectl get deployment my-api -n dev
# READY: 0/0
```

## 6. Use it again

Send it a request. The [doorman](../concepts/wake-on-request.md) holds the request, wakes the workload, and answers once it's Ready:

```bash
kubectl run curl --rm -it --image=curlimages/curl:8.7.1 -n dev --restart=Never -- curl -s http://my-api
```

A browser opening it gets a waking-up page instead, which loads the app once it's up. You can also wake it from the terminal:

```bash
kubectl hybernate wake my-api -n dev
```

## 7. Stop managing it

```bash
kubectl label deployment my-api -n dev hybernate.io/managed-
```

Removing the label removes the ManagedWorkload. If the workload is paused, it's scaled back to its replicas first, so it's never left switched off.

## What's next?

- [Opting In](../guides/opt-in.md): every setting, namespaces, and GitOps
- [Idle Detection](../concepts/idle-detection.md): how the activity clock decides
- [Wake on Request](../concepts/wake-on-request.md): how requests wake a paused workload
- [ManagedWorkload Guide](../guides/managed-workload.md): writing a ManagedWorkload yourself, for settings annotations don't cover
