# Hybernate

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.30+-326CE5?logo=kubernetes&logoColor=white)](https://kubernetes.io)

**Your Kubernetes workloads are running 24/7. Your users aren't.**

Hybernate is a Kubernetes operator that detects idle workloads, learns their demand patterns, and automatically pauses them, scaling to zero without deleting anything. It brings them back on the first request or before traffic returns, turning your non-production clusters from always-on cost centers into pay-for-what-you-use environments.

## Why Hybernate?

Most staging, dev, and test workloads sit idle 60-80% of the time: nights, weekends, holidays. You're paying for compute that nobody is using.

Hybernate fixes this by:

- **Detecting idle workloads** with a per-workload activity clock: CPU, deploys, Prometheus queries, and activity annotations from your own tooling, with no learning period
- **Learning demand patterns** via a per-workload Holt-Winters forecasting model that tracks daily and weekly seasonality
- **Acting automatically** by pausing idle workloads and resuming proactively before users arrive
- **Tracking savings** with per-workload cost accounting on what pods request, the resources each pause frees, and a cluster-wide total in `kubectl hybernate status`

## How It Works

```mermaid
flowchart TD
    label["Deployment, StatefulSet, or namespace<br/>labelled hybernate.io/managed: &quot;true&quot;"] --> mw["ManagedWorkload<br/>settings from annotations"]
    mw --> running["Running<br/>activity clock: CPU, deploys,<br/>activity annotations, Prometheus"]
    running -- "no activity for idleAfter,<br/>and no forecast of demand" --> paused["Paused<br/>scaled to zero; Services<br/>routed to the doorman"]
    paused -- "a request, an activity annotation,<br/>or autoResume ahead of demand" --> resuming["Resuming<br/>replicas restored,<br/>dependencies woken"]
    resuming -- "pods Ready;<br/>held requests passed through" --> running
```

1. You label a Deployment, StatefulSet, or namespace `hybernate.io/managed: "true"`
2. The operator tracks when the workload was last active: CPU, deploys, and activity annotations
3. A per-workload forecast model learns when the workload is typically busy
4. When nothing has been active for `idleAfter` (default 1 hour), the workload is paused
5. Before the next busy period, the forecast triggers an automatic resume

## Quick Start

**See what's idle across your cluster, before installing anything:**

```bash
kubectl krew install --manifest-url \
  https://github.com/okedeji/hybernate/releases/latest/download/krew-hybernate.yaml
kubectl hybernate scan
```

**Install the operator:**

```bash
helm install hybernate oci://ghcr.io/okedeji/charts/hybernate \
  --version 0.2.0 \
  --namespace hybernate-system \
  --create-namespace
```

Hybernate supports Kubernetes 1.30 and later, and is tested in CI on 1.30 and 1.37. The chart installs the operator, the doorman that wakes paused workloads on request, and the ManagedWorkload CRD.

**Opt a workload in, measuring first:**

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-api
  namespace: staging
  labels:
    hybernate.io/managed: "true"
  annotations:
    hybernate.io/dry-run: "true"
    hybernate.io/idle-after: "2h"
```

The label opts it in; the annotations are its settings. Label a namespace instead to cover every workload in it. In dry-run, Hybernate tracks the workload's activity and what pausing would save, without pausing it. When you're confident:

```bash
kubectl hybernate enable my-api -n staging
```

## Features

### Core

- **Activity-based idle detection**: any sign of use (CPU, a deploy, a Prometheus query such as request rate, an activity annotation) keeps a workload awake; it pauses after `idleAfter` with none
- **Demand forecasting** via a Holt-Winters double seasonal model that learns daily and weekly patterns per workload, with confidence scoring and anomaly detection
- **Works with HPA, KEDA, Argo CD, and Flux**: an HPA leaves a paused workload at zero, a KEDA one is held there through KEDA, and a GitOps tool undoing a pause is reported with the fix rather than fought
- **Wake on request**: a request to a paused workload's Service wakes it and is held until the workload is Ready, so callers see a slow response instead of an error. Browsers get a waking-up page that reloads until the app is up
- **Pause and resume** with scale to zero and forecast-driven resume. Hybernate never deletes a workload or its storage

### Operations

- **Label opt-in**: `hybernate.io/managed: "true"` on a workload or namespace, with settings as annotations, all in the manifests you already keep in Git
- **`kubectl hybernate scan`** finds idle workloads in a cluster and what they cost, with nothing installed, and with Prometheus replays the last week to show how long each would have slept and what that frees, in a report that opens in your browser to share
- **`kubectl hybernate status`** shows what Hybernate is doing in a cluster on one screen: each workload's state, when it pauses next or what holds it awake, what needs attention, what it has saved, and the latest pauses and wakes with what caused them
- **`kubectl hybernate pause`** pauses a workload now, such as a preview environment at the end of the day, to wake on its next request
- **`kubectl hybernate wake`** wakes a paused workload from the terminal and waits until it's Running
- **`kubectl hybernate enable`** ends dry-run once you trust what you've measured, and says what to change in Git when Argo CD or Flux applies the workload
- **Cost tracking** per workload, for the current month: what it cost, what pausing saved, and a projection for the month, priced at the list price of the nodes it runs on
- **Dry-run mode** to observe every decision the operator would make without it taking action

### Observability

- **Prometheus metrics** for operator health, lifecycle transitions, wakes on request, and prediction state, with alerting rules in the Helm chart
- **Kubernetes events** for every state change, visible in `kubectl describe`, and status conditions that say why a workload isn't paused or woken

## Architecture

```mermaid
flowchart TB
    subgraph operator["Operator Deployment"]
        optin["Opt-in reconcilers<br/>Deployments, StatefulSets"]
        mwr["ManagedWorkload reconciler<br/>activity clock, pause and resume,<br/>dependencies, cost"]
        forecast["Forecast engine<br/>one per workload"]
        optin --> mwr
        mwr --- forecast
    end
    subgraph doorman["Doorman Deployment"]
        door["Holds requests to paused workloads,<br/>wakes them, passes requests through"]
    end
    clients["Clients, ingress controllers,<br/>load balancers"] -- "Service of a paused workload" --> door
    door -- "hybernate.io/last-request" --> api
    mwr --> api["Kubernetes API server"]
    mwr --> ms["metrics-server"]
    mwr -.-> prom["Prometheus<br/>optional"]
```

| Component | Description |
|-----------|-------------|
| **ManagedWorkload** | Per-workload CR that defines idle policy, wake behaviour, and cost tracking |
| **Opt-in controller** | Creates and updates a ManagedWorkload for each labelled workload, from its annotations |
| **Doorman** | Holds requests to a paused workload's Services, wakes it, and passes them through once it's Ready |
| **Forecast Engine** | Per-workload Holt-Winters model that learns demand patterns, holds off a pause when demand is expected, and wakes workloads ahead of it |

## Documentation

Full docs at **[okedeji.io/hybernate](https://okedeji.io/hybernate)**

- [Installation](https://okedeji.io/hybernate/getting-started/installation/): Helm, kubectl, and source
- [Quickstart](https://okedeji.io/hybernate/getting-started/quickstart/): manage your first workload
- [Opting In](https://okedeji.io/hybernate/guides/opt-in/): the label, every setting, and GitOps
- [Idle Detection](https://okedeji.io/hybernate/concepts/idle-detection/): how the activity clock decides when to pause
- [Forecasting](https://okedeji.io/hybernate/concepts/forecasting/): the Holt-Winters prediction engine
- [Cost Tracking](https://okedeji.io/hybernate/concepts/cost-tracking/): what a workload costs, and what pausing saves
- [API Reference](https://okedeji.io/hybernate/reference/api/): complete CRD field reference
- [Metrics Reference](https://okedeji.io/hybernate/reference/metrics/): all Prometheus metrics

## Contributing

We welcome contributions. See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, coding standards, and PR guidelines.

## Security

To report a security vulnerability, see [SECURITY.md](SECURITY.md).

## License

Copyright 2026. Licensed under the [Apache License, Version 2.0](LICENSE).
