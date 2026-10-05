# Data and Access

Everything the Hybernate operator and its doorman read, write, and send, for a security review. The [RBAC](helm-values.md#rbac) the Helm chart grants matches this list; with `watchNamespaces`, all of it but Namespaces and Nodes is limited to those namespaces.

## What it reads

| Resource | Why | How |
|---|---|---|
| Deployments, StatefulSets | The workloads it manages: replicas, pod template, labels, and annotations | Watched |
| Pods | What a workload's pods request, sidecars included, to price it; and which pod sent a request that woke a workload | Read when needed, not cached |
| Pod metrics (`metrics.k8s.io`) | CPU use, the activity clock's main signal | Read when needed, not cached |
| Services, EndpointSlices | Where to route a paused workload's requests, and which workload an address in an environment names | Watched |
| ConfigMaps | Only the ones a managed workload takes environment variables from, to learn its dependencies | Read one by one, not cached; never written |
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
| KEDA ScaledObjects | The `autoscaling.keda.sh/paused-replicas` annotation, while their workload is paused |
| EndpointSlices | Its own, routing a paused workload's Services to the doorman; deleted when the workload wakes |
| ManagedWorkloads | Its own status, including each workload's forecast state, the ones it creates for labelled workloads, and the doorman's wake annotations |
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

## The plugin

`kubectl hybernate` runs with your own kubeconfig and permissions, reads the same kinds of resources and their events, and changes nothing except for `wake` (an annotation on a ManagedWorkload) and `enable` (the `hybernate.io/dry-run` annotation, on a workload or its namespace). `scan` reads Prometheus through the API server's service proxy, or at `--prometheus-url`, and writes its report only to your machine.
