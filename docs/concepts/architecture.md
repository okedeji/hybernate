# Architecture

Hybernate is a Kubernetes operator built on [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime). It runs as one binary inside your cluster, in two Deployments: the operator, which manages workloads through two kinds of reconciliation loop, and the [doorman](wake-on-request.md), which holds requests to paused workloads and wakes them.

## Components

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

## Reconcilers

### ManagedWorkload Reconciler

The primary reconciler. Watches `ManagedWorkload` CRs and drives each workload through its lifecycle. On every reconcile it:

1. Checks for another ManagedWorkload managing the same target
2. Checks that the target Deployment or StatefulSet exists and isn't labelled `hybernate.io/ignore`; an ignored one is let go of, scaled back up first if it's paused
3. Lets go of a workload whose namespace is protected, that is Hybernate's own operator or doorman, or that is a StatefulSet deleting its PVCs on scale-down, scaled back up first if it's paused
4. Wakes a paused workload scaled up outside Hybernate
5. Reports the HPA or KEDA ScaledObject that scales the workload
6. Learns the workload's dependencies from its environment, and from the requests that woke it
7. Routes a paused workload's Services to the doorman, or stops routing them
8. Finishes a pause or resume an earlier reconcile started, taking a pause up again from the target's `hybernate.io/paused-from` if status lost it, and wakes a paused workload put in dry-run
9. Leaves alone a target scaled to zero outside Hybernate
10. Acts on a pause someone asked for with `kubectl hybernate pause`
11. Runs the activity clock and the forecast: records activity, and pauses once there has been none for `idleAfter` and no demand is forecast; for a paused workload, checks whether anything, or `autoResume`, asks it to wake
12. Adds up what dry-run held back, accumulates cost, and writes status

Each ManagedWorkload gets its own forecast engine, kept in memory and saved to the ManagedWorkload's `status.prediction.state` as it learns each hour, so it survives operator restarts and leader changes. Every reconcile is bounded by a 2-minute timeout, and `--max-concurrent-reconciles` (8 by default) workloads are reconciled at once, so one slow metrics or Prometheus query doesn't hold up another workload's wake. Reconciles wait in one queue ordered by priority: what wakes a workload (a request the doorman holds, an activity or pause annotation, a scale-up from zero) and a resume under way come first, then other changes, and the periodic checks of awake and paused workloads last, so a wake isn't queued behind every workload's metrics and Prometheus checks.

### Opt-In Reconcilers

One for Deployments and one for StatefulSets. Each watches its workloads, their namespaces, and the ManagedWorkloads created from them. When a workload or its namespace is labelled `hybernate.io/managed: "true"`, it creates a ManagedWorkload named after the workload (or `<name>-<kind>` when that name is taken) and owned by it, with a spec built from the workload's annotations, then its namespace's, then the cluster-wide defaults. It keeps the fields the annotations set in step with them, leaves the rest as you set them, and deletes the ManagedWorkload when the label goes. A ManagedWorkload someone wrote for the same workload always wins.

## Internal Packages

| Package | Responsibility |
|---------|---------------|
| `internal/controller` | The ManagedWorkload and opt-in reconcilers |
| `internal/forecast` | Holt-Winters model, phase lifecycle, confidence scoring, anomaly detection |
| `internal/signal` | Prometheus PromQL activity queries |
| `internal/autoscaler` | Finds a workload's HPA or KEDA ScaledObject, and holds a KEDA one at zero while it's paused |
| `internal/gitops` | Tells from field managers whether a GitOps tool (Argo CD, Flux, Fleet, kapp or Kluctl) sets a workload's replicas, and how to tell it to leave them |
| `internal/lifecycle` | Pause and resume, through the scale subresource |
| `internal/discovery` | The cluster scan behind `kubectl hybernate scan` |
| `internal/doorman` | Holds requests to a paused workload's Service and wakes it |
| `internal/cost` | Cost rates, node list prices, and monthly projection |
| `internal/metrics` | Prometheus metric definitions and K8s Metrics API reader |

## External Dependencies

Hybernate reads from three external systems:

- **Kubernetes API Server**: for scaling Deployments and StatefulSets, routing Services' EndpointSlices to the doorman, and reading what pricing and dependencies need: pods, PVCs, nodes' labels, ConfigMaps, HPAs and KEDA ScaledObjects
- **Metrics Server**: for pod CPU usage (required)
- **Prometheus**: for PromQL activity queries (optional)

## Node-Level Cost Savings

Hybernate operates at the workload layer. It does not manage nodes directly. When Hybernate pauses or scales down a workload, it frees CPU and memory on the node. A cluster autoscaler (Cluster Autoscaler, Karpenter, or a managed equivalent) is responsible for detecting underutilized nodes and removing them to realize actual infrastructure cost savings.

See the [Cluster Autoscaler Guide](../guides/cluster-autoscaler.md) for recommended configurations.

## Leader Election

When running multiple operator replicas for high availability, Hybernate uses controller-runtime's leader election (`--leader-elect`). Only the leader runs reconciliation loops; standby replicas take over if the leader fails. The doorman doesn't elect a leader: every replica serves traffic.

## Security Model

- The operator runs as a non-root user in a distroless container
- RBAC is scoped to the minimum required permissions; see [Data and Access](../reference/data-and-access.md#rbac)
- Metrics are served over HTTPS by default with authentication
- HTTP/2 is disabled by default to mitigate known vulnerabilities
