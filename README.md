# Hybernate

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.26+-326CE5?logo=kubernetes&logoColor=white)](https://kubernetes.io)

**Intelligent Kubernetes workload lifecycle management.**

Hybernate is a Kubernetes operator that automatically pauses, scales, and destroys workloads based on real-time idle detection and demand forecasting, saving you money without manual intervention.

## Features

- **Demand Forecasting**: Holt-Winters double seasonal model learns daily and weekly traffic patterns per workload
- **Multi-Signal Idle Detection**: CPU metrics + Prometheus queries with consensus-based confirmation and configurable grace periods
- **Prediction-Driven Scaling**: replica counts adjust based on forecasted demand with stabilization windows, step limits, and guard probes
- **Pause & Destroy Lifecycle**: scale to zero with automatic expiry, resume, and PVC retention
- **Cost Tracking**: per-workload resource cost and savings calculation with cluster-wide aggregation
- **Auto-Discovery**: scan namespaces, classify workloads as Active/Idle/Wasteful, auto-create management resources
- **GitOps Export**: `kubectl hybernate export` generates manifests for ArgoCD/Flux workflows
- **Dry Run Mode**: observe what Hybernate would do without it taking action
- **Full Observability**: 30+ Prometheus metrics, Grafana dashboards, and alerting rules included

## Quick Start

```bash
# Install the operator
kubectl apply -f https://github.com/okedeji/hybernate/releases/latest/download/install.yaml

# Create a ManagedWorkload
cat <<EOF | kubectl apply -f -
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
    expireAfter: "24h"
    expireAction: Resume
  prediction:
    confidence: 85
  costTracking:
    enabled: true
  dryRun: true
EOF

# Watch what happens
kubectl describe managedworkload my-api -n staging
```

## Documentation

Full documentation is available at [okedeji.github.io/hybernate](https://okedeji.github.io/hybernate).

| Section | Description |
|---------|-------------|
| [Installation](https://okedeji.github.io/hybernate/getting-started/installation/) | Helm, kubectl, and source install |
| [Quickstart](https://okedeji.github.io/hybernate/getting-started/quickstart/) | Manage your first workload in 5 minutes |
| [Architecture](https://okedeji.github.io/hybernate/concepts/architecture/) | System overview and components |
| [ManagedWorkload Guide](https://okedeji.github.io/hybernate/guides/managed-workload/) | Full spec reference with examples |
| [API Reference](https://okedeji.github.io/hybernate/reference/api/) | Complete CRD field reference |
| [Metrics](https://okedeji.github.io/hybernate/reference/metrics/) | Prometheus metrics reference |

## How It Works

```
ManagedWorkload CR ──► Reconciler Loop ──► Idle Detection
                                      ──► Forecast Engine (Holt-Winters)
                                      ──► Scaling Constraints
                                      ──► Pause / Resume / Destroy
                                      ──► Cost Tracking
```

1. You declare a `ManagedWorkload` pointing at a Deployment or StatefulSet
2. The operator monitors CPU usage and optional Prometheus signals
3. A per-workload Holt-Winters model learns daily and weekly demand patterns
4. When the workload is confirmed idle (signals + prediction + grace period), it's paused
5. Cost savings are tracked and reported via Prometheus metrics and the `HybernateReport` singleton

## Contributing

See [CONTRIBUTING](https://okedeji.github.io/hybernate/contributing/) for development setup, testing, and PR guidelines.

## License

Copyright 2026. Licensed under the [Apache License, Version 2.0](LICENSE).
