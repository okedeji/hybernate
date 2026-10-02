# Hybernate

**Intelligent Kubernetes workload lifecycle management.**

Hybernate is a Kubernetes operator that predicts your workload demand using Holt-Winters forecasting and pauses, resumes, or destroys workloads to cut infrastructure costs. It pauses a workload once nothing has used it for a configurable time, and supports dry run mode so you can observe its recommendations and build confidence before letting it drive actions.

---

## Why Hybernate?

Most Kubernetes clusters run workloads 24/7, even when no one is using them. Dev environments sit idle overnight. Staging clusters burn resources on weekends. Production services stay at peak capacity long after traffic drops.

Hybernate fixes this by learning your workload patterns and acting on them:

- **Idle workloads get paused.** They are scaled to zero when no one is using them and resumed automatically when demand returns.
- **Abandoned workloads get cleaned up.** They are destroyed after extended idle periods, with PVC retention for safety.

## Key Features

### Demand Forecasting
A Holt-Winters double seasonal model learns daily and weekly traffic patterns for each workload. After observing your traffic for a few hours, it starts predicting demand, and its confidence improves over time. If traffic patterns shift, a built-in anomaly detector notices the drift, demotes the model's confidence, and re-learns from the new baseline.

### Activity Clock
Hybernate records the last time each workload was in use: CPU above a threshold, a deploy, or an activity annotation from your own tooling. Once nothing has been active for `idleAfter`, it pauses the workload. There's no learning period.

### Safe by Default
Hybernate never pauses a workload it can't measure, and a confident forecast can hold off a pause. Enable `dryRun` mode to see what Hybernate would do without it actually doing anything. Conflict detection catches external changes to your workloads.

### Cost Tracking
Track per-workload resource consumption and savings. See exactly how much you're saving from paused and destroyed workloads.

### Auto-Discovery
WorkloadPolicy scans your namespaces, classifies workloads as Active or Idle, and can auto-create `ManagedWorkload` resources for the ones that need attention.

### GitOps-Native Export
Use `kubectl hybernate export` to generate ManagedWorkload manifests from discovered workloads, ready to commit to Git and deploy via ArgoCD or Flux.

### Full Observability
Prometheus metrics for operator health, lifecycle transitions, and prediction confidence, with alerting rules included.

---

## How It Works

![How It Works](assets/how-it-works.png)

---

## Quick Example

```yaml title="managedworkload.yaml" linenums="1"
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata:
  name: my-api
  namespace: sandbox
spec:
  target:
    kind: Deployment
    name: my-api
  idlePolicy:
    action: pause
    idleAfter: 1h
  prediction:
    confidence: 85
```

This watches the `my-api` Deployment and pauses it when both CPU and memory stay below 10% of their respective requests for 10 minutes. The forecast engine learns daily and weekly patterns and must agree before any action is taken.

For idle policies, cost rate overrides, and the full spec, see the [ManagedWorkload Guide](guides/managed-workload.md).

---

## Getting Started

Ready to try it? Head to the [Installation](getting-started/installation.md) guide, then follow the [Quickstart](getting-started/quickstart.md) to manage your first workload in under 5 minutes.
