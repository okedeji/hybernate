# Architecture

Hybernate is a Kubernetes operator built on [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime). It runs as a single binary inside your cluster and manages workloads through three reconciliation loops.

## Components

![Architecture](../assets/architecture.png)

## Three Reconcilers

### ManagedWorkload Reconciler

The primary reconciler. Watches `ManagedWorkload` CRs and drives each workload through its lifecycle. On every reconcile it:

1. Validates the target Deployment/StatefulSet exists
2. Detects and handles external replica drift
3. Processes manual overrides (`desiredState`)
4. Checks pause expiry and PVC retention cleanup
5. Runs idle detection (signals + prediction + grace period)
6. Evaluates scaling decisions
7. Accumulates cost data

Each ManagedWorkload gets its own forecast engine instance, serialized into the CR status so it survives operator restarts.

### WorkloadPolicy Reconciler

Watches `WorkloadPolicy` CRs. On each reconcile it scans the namespace for Deployments and StatefulSets, fetches their metrics from the Kubernetes Metrics API, and classifies each as Active, Idle, or Wasteful.

In `auto-manage` mode, it creates `ManagedWorkload` CRs for idle and wasteful workloads using the policy's default settings.

### HybernateReport Reconciler

Watches the cluster-scoped `HybernateReport` singleton. Aggregates counts and cost data across all ManagedWorkloads and publishes cluster-wide Prometheus metrics.

## Internal Packages

| Package | Responsibility |
|---------|---------------|
| `internal/controller` | Reconciliation logic for all three CRDs |
| `internal/forecast` | Holt-Winters model, phase lifecycle, confidence scoring, anomaly detection |
| `internal/policy` | Idle state machine, scaling constraint evaluation |
| `internal/signal` | Signal interface, CPU threshold checker, Prometheus PromQL prober |
| `internal/lifecycle` | Pause, resume, scale, destroy operations against K8s API |
| `internal/discovery` | Namespace scanning and workload classification |
| `internal/cost` | Cost accumulation and estimation |
| `internal/metrics` | Prometheus metric definitions and K8s Metrics API reader |
| `internal/export` | ManagedWorkload YAML generation from discovered workloads |

## External Dependencies

Hybernate reads from three external systems:

- **Kubernetes API Server**: for managing Deployments, StatefulSets, PVCs, and CRs
- **Metrics Server**: for pod CPU and memory usage (required)
- **Prometheus**: for custom PromQL signal queries (optional)

## Node-Level Cost Savings

Hybernate operates at the workload layer. It does not manage nodes directly. When Hybernate pauses or scales down a workload, it frees CPU and memory on the node. A cluster autoscaler (Cluster Autoscaler, Karpenter, or a managed equivalent) is responsible for detecting underutilized nodes and removing them to realize actual infrastructure cost savings.

See the [Cluster Autoscaler Guide](../guides/cluster-autoscaler.md) for recommended configurations.

## Leader Election

When running multiple replicas for high availability, Hybernate uses controller-runtime's leader election (`--leader-elect`). Only the leader runs reconciliation loops; standby replicas take over if the leader fails.

## Security Model

- The operator runs as a non-root user in a distroless container
- RBAC is scoped to the minimum required permissions (see `config/rbac/`)
- Metrics are served over HTTPS by default with authentication
- HTTP/2 is disabled by default to mitigate known vulnerabilities
