# Data and Access

Everything the Hybernate operator and its doorman read, write, and send, for a security review. The [RBAC](#rbac) the Helm chart grants matches this list.

## What it reads

| Resource | Why | How |
|---|---|---|
| Deployments, StatefulSets | The workloads it manages: replicas, pod template, labels, and annotations | Watched |
| Pods | What a workload's pods request, sidecars included, to price it; and which pod sent a request that woke a workload | Read when needed, not cached |
| Pod metrics (`metrics.k8s.io`) | CPU use, the activity clock's main signal | Read when needed, not cached |
| Services | Which Services to route a paused workload's requests through, and which workload an address in an environment names | Watched |
| EndpointSlices | Its own, routing paused workloads to the doorman, and the doorman Service's, to find the doorman's pods. The doorman reads every slice in the watched namespaces, to find a woken workload's Ready pods | Watched |
| ConfigMaps | Only the ones a managed workload takes environment variables from, to learn its dependencies | Read one by one with `get`, not cached; never listed or written |
| PersistentVolumeClaims | Their size, to price the storage a paused workload keeps | Watched |
| HorizontalPodAutoscalers, KEDA ScaledObjects | What scales a workload, and its range | Watched |
| Namespaces | Their labels: opt-in, settings, and protection | Watched |
| Nodes | Their names and labels only (instance type, region, spot), to price workloads | Metadata watched |
| ManagedWorkloads | Its own configuration | Watched |

**Never read:** Secrets, of any kind, anywhere. An address set in a Secret is learned only from the request it's used for.

## What it writes

| Resource | What |
|---|---|
| Deployments, StatefulSets | Their replica count, through the scale subresource, and nothing else: no other field, and never a delete |
| KEDA ScaledObjects | The `autoscaling.keda.sh/paused-replicas` annotation, while their workload is paused or resuming; afterwards the value it had before, or none |
| EndpointSlices | Its own, routing a paused workload's Services to the doorman; deleted when the workload wakes |
| ManagedWorkloads | Its own status, including each workload's forecast state; the ones it creates for labelled workloads; `hybernate.io/last-activity` on a dependency it wakes; and, from the doorman, `hybernate.io/last-request` and `hybernate.io/last-request-from` (the caller's IP address) on one a request wakes |
| Events | On ManagedWorkloads and workloads, to say what it did and why |
| Leases | Leader election, in its own namespace |

**Never written:** any other resource, any other field of a workload, and nothing is ever deleted but its own ManagedWorkloads and EndpointSlices.

## What it sends

Nothing leaves the cluster, unless configured to:

| Destination | When |
|---|---|
| The Kubernetes API server | Always: everything above |
| Prometheus, at `prometheus.url` | Only with Prometheus activity queries configured; it sends the queries and reads their results |

No telemetry, no usage reporting, no update checks. The doorman answers requests to paused workloads and forwards them to the woken pod; it doesn't store or log their contents.

## RBAC

What the Helm chart grants, for a release named `hybernate`; resource names start with the release's full name. Without `watchNamespaces`, the rules marked _namespaced_ are in ClusterRoles; with it, they're in a Role in each watched namespace, and nothing marked _namespaced_ is granted anywhere else.

**Operator** (ServiceAccount `hybernate`), ClusterRole and ClusterRoleBinding `hybernate-manager`:

| API group | Resources | Verbs | |
|---|---|---|---|
| `""` | `namespaces`, `nodes` | `get`, `list`, `watch` | always cluster-wide |
| `""` | `configmaps` | `get` | namespaced |
| `""` | `events` | `create`, `patch` | namespaced |
| `events.k8s.io` | `events` | `create`, `patch` | namespaced |
| `""` | `services` | `get`, `list`, `watch` | namespaced |
| `""` | `pods` | `get`, `list` | namespaced |
| `""` | `persistentvolumeclaims` | `get`, `list`, `watch` | namespaced |
| `discovery.k8s.io` | `endpointslices` | `get`, `list`, `watch`, `create`, `update`, `patch`, `delete` | namespaced |
| `apps` | `deployments`, `statefulsets` | `get`, `list`, `watch` | namespaced |
| `apps` | `deployments/scale`, `statefulsets/scale` | `get`, `update` | namespaced |
| `autoscaling` | `horizontalpodautoscalers` | `get`, `list`, `watch` | namespaced |
| `keda.sh` | `scaledobjects` | `get`, `list`, `watch`, `patch` | namespaced |
| `hybernate.io` | `managedworkloads` | `get`, `list`, `watch`, `create`, `update`, `patch`, `delete` | namespaced |
| `hybernate.io` | `managedworkloads/status` | `get`, `update`, `patch` | namespaced |
| `hybernate.io` | `managedworkloads/finalizers` | `update` | namespaced |
| `metrics.k8s.io` | `pods` | `get`, `list` | namespaced |

With `watchNamespaces` and the doorman enabled, a Role `hybernate-doorman-endpoints` in the release namespace lets the operator `get`, `list` and `watch` `endpointslices` there, to find the doorman's pods.

With leader election, a Role `hybernate-leader-election` in the release namespace grants `coordination.k8s.io` `leases` (`get`, `create`, `update`) and `""` `events` (`create`, `patch`).

**Doorman** (ServiceAccount `hybernate-doorman`), ClusterRole and ClusterRoleBinding `hybernate-doorman`, or a Role in each watched namespace:

| API group | Resources | Verbs |
|---|---|---|
| `hybernate.io` | `managedworkloads` | `get`, `list`, `watch`, `patch` |
| `discovery.k8s.io` | `endpointslices` | `get`, `list`, `watch` |
| `""`, `events.k8s.io` | `events` | `create`, `patch` |

**Metrics**, with `metrics.secure` (the default): ClusterRoleBindings `hybernate-auth-delegator` and `hybernate-doorman-auth-delegator` to the built-in `system:auth-delegator` ClusterRole, so the operator and the doorman can check the tokens of whoever scrapes them; and, with `metrics.enabled`, a ClusterRole `hybernate-metrics-reader` allowing `get` on the `/metrics` URL, bound to `metrics.readerSubjects` by a ClusterRoleBinding of the same name.

## The plugin

`kubectl hybernate` runs with your own kubeconfig and permissions, reads the same kinds of resources and their events, and changes nothing except for `wake` (activity annotations on a ManagedWorkload) and `enable` (the `hybernate.io/dry-run` annotation, on a workload or its namespace, or `spec.dryRun` on a ManagedWorkload you wrote). `scan` reads Prometheus through the API server's service proxy, or at `--prometheus-url`, and writes its report only to your machine, in a file only you can read.
