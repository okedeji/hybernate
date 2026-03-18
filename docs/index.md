# Hybernate

**Intelligent Kubernetes workload lifecycle management.**

Hybernate is a Kubernetes operator that automatically pauses, scales, and destroys workloads based on real-time idle detection and demand forecasting — saving you money without manual intervention.

---

## Why Hybernate?

Most Kubernetes clusters run workloads 24/7, even when no one is using them. Dev environments sit idle overnight. Staging clusters burn resources on weekends. Production services stay at peak capacity long after traffic drops.

Hybernate fixes this by learning your workload patterns and acting on them:

- **Idle workloads get paused** — scaled to zero when no one is using them, resumed automatically when demand returns.
- **Over-provisioned workloads get right-sized** — replica counts adjust to match actual demand, not worst-case guesses.
- **Abandoned workloads get cleaned up** — destroyed after extended idle periods, with PVC retention for safety.

## Key Features

### Demand Forecasting
A Holt-Winters double seasonal model learns daily and weekly traffic patterns for each workload. After observing your traffic for a few hours, it starts predicting demand — and its confidence improves over time.

### Multi-Signal Idle Detection
CPU metrics alone aren't enough. Hybernate combines Kubernetes metrics with optional Prometheus queries (active connections, queue depth, HTTP request rates) and requires **all signals to agree** before taking action.

### Safe by Default
Every action goes through a grace period, signal consensus, and confidence threshold. Enable `dryRun` mode to see what Hybernate would do without it actually doing anything. Conflict detection catches external changes to your workloads.

### Cost Tracking
Track per-workload resource consumption and savings. See exactly how much you're saving from paused, scaled, and destroyed workloads — aggregated across your entire cluster via the HybernateReport.

### Auto-Discovery
WorkloadPolicy scans your namespaces, classifies workloads as Active, Idle, or Wasteful, and can auto-create ManagedWorkload resources for the ones that need attention.

### GitOps-Native Export
Use `kubectl hybernate export` to generate ManagedWorkload manifests from discovered workloads — ready to commit to Git and deploy via ArgoCD or Flux.

### Full Observability
Prometheus metrics for every lifecycle transition, prediction confidence score, cost saving, and scale event. Grafana dashboards and alerting rules included.

---

## How It Works

```
                    ┌─────────────────────────────────┐
                    │        ManagedWorkload CR        │
                    │  (target, idle policy, scale     │
                    │   policy, cost tracking, ...)    │
                    └──────────────┬──────────────────┘
                                   │
                    ┌──────────────▼──────────────────┐
                    │         Reconciler Loop          │
                    │                                  │
                    │  1. Validate target              │
                    │  2. Detect drift                 │
                    │  3. Check idle signals           │
                    │  4. Consult forecast engine      │
                    │  5. Apply scaling / pause        │
                    │  6. Track costs                  │
                    └──────────────┬──────────────────┘
                                   │
              ┌────────────────────┼────────────────────┐
              │                    │                    │
     ┌────────▼───────┐  ┌────────▼───────┐  ┌────────▼───────┐
     │  Idle → Pause   │  │  Scale Up/Down │  │  Destroy +     │
     │  (grace period) │  │  (constraints) │  │  PVC Cleanup   │
     └────────────────┘  └────────────────┘  └────────────────┘
```

---

## Quick Example

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
    gracePeriod: "10m"
  scalePolicy:
    minReplicas: 1
    maxReplicas: 10
  pause:
    expireAfter: "24h"
    expireAction: Resume
  costTracking:
    enabled: true
```

This tells Hybernate: "Watch the `my-api` Deployment. If CPU drops below 50m for 10 minutes, pause it. Auto-resume after 24 hours. Track how much it costs."

---

## Getting Started

Ready to try it? Head to the [Installation](getting-started/installation.md) guide, then follow the [Quickstart](getting-started/quickstart.md) to manage your first workload in under 5 minutes.
