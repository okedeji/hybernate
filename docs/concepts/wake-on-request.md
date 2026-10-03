# Wake on Request

A paused workload has no pods, so nothing answers its Service. With wake on request, the first request wakes it instead: the connection is held while the workload starts, then passed to the first Ready pod. The caller sees one slow response, not an error.

It's on by default for every workload Hybernate pauses on its own. There's nothing to configure.

## How it works

Hybernate runs a small proxy, the **doorman**, as its own Deployment (two replicas) next to the operator.

1. When a workload pauses, the operator finds every Service that selects its pods and adds one extra EndpointSlice to each, pointing at the doorman. The Service, its ClusterIP, and its DNS name don't change, so callers and Ingresses need no changes.
2. A connection to the Service reaches the doorman. The doorman holds it and sets `hybernate.io/last-request` on the ManagedWorkload, which wakes it the same way as an [activity annotation](idle-detection.md#activity-annotations). A `WokenByRequest` event names the Service, and once the workload is Running, its clock shows `lastActivitySource: request`.
3. The doorman watches the Service's own EndpointSlices. As soon as a pod is Ready, it connects to that pod and passes the held connection through, including any bytes the caller already sent.
4. Once the workload is `Running`, the operator removes the doorman's EndpointSlice and traffic goes straight to the pods again.

The doorman works on TCP connections, not HTTP requests, so it handles any TCP protocol: HTTP, gRPC, Postgres, Redis. Waking a workload also wakes its [dependencies](dependencies.md).

Ingress controllers, Gateway API implementations, and cloud load balancers that read EndpointSlices reach the doorman the same way. See [Compatibility](../reference/compatibility.md) for which have been checked.

```
WakeOnRequest=True   DoormanRouted   requests are held and wake the workload
```

## How long a connection is held

The doorman holds each connection for up to `wake.maxWait`, 2 minutes by default. If no pod is Ready by then, it closes the connection. The wake carries on, so a retry a little later succeeds.

```yaml
spec:
  wake:
    maxWait: 5m     # a slow starter, such as a database replaying its log
```

Callers have their own timeouts. Most HTTP clients wait at least 30 seconds, and browsers wait minutes. A proxy in front of the Service can give up sooner: ingress-nginx closes an upstream request after 60 seconds by default. For a workload that takes longer than that to start, raise the Ingress timeout to match `maxWait`:

```yaml
metadata:
  annotations:
    nginx.ingress.kubernetes.io/proxy-read-timeout: "120"
```

## Turning it off

```yaml
spec:
  wake:
    onRequest: false
```

Requests to the paused workload then fail, as they would without Hybernate. To turn it off for the whole cluster, set the Helm value `doorman.enabled: false`, or run the operator with `--doorman-service=""`.

## When a workload isn't routed

| Situation | What happens |
|-----------|--------------|
| `desiredState: Paused` | Not routed. A manual pause only ends when you change it, so holding the connection would only delay the error |
| No Service selects the workload | `WakeOnRequest=False`, reason `NoServices` |
| A headless Service (`clusterIP: None`) | Not routed. Callers resolve pod IPs from DNS and never reach the doorman; wake the workload with an [activity annotation](idle-detection.md#activity-annotations) instead |
| A Service without a selector | Not routed, since its endpoints are managed by hand |
| UDP or SCTP ports | Not routed; only TCP ports are |
| A Service behind GKE container-native load balancing (`cloud.google.com/neg`) | Not routed. `WakeOnRequest` names it, reason `UnsupportedLoadBalancer` if no Service is routed, and a warning event is emitted. See [Compatibility](../reference/compatibility.md#cloud-load-balancers) |
| No doorman pod is Ready | `WakeOnRequest=False`, reason `DoormanUnavailable`. Requests fail until a doorman pod is back |
| All doorman ports are in use | `WakeOnRequest=False`, reason `NoFreePort`. The doorman has 10,000 ports, one per paused Service port in the cluster |

## Network policies

The doorman connects to woken pods directly, from the Hybernate namespace. If a workload's namespace has a default-deny NetworkPolicy, allow ingress from the doorman:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-hybernate-doorman
  namespace: sandbox-42
spec:
  podSelector: {}
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels: {kubernetes.io/metadata.name: hybernate-system}
          podSelector:
            matchLabels: {control-plane: doorman}
```

Callers reach the doorman on ports 20000-29999, so a default-deny policy on the Hybernate namespace needs to allow those too.

## What the caller sees

- **A pod in the cluster or an Ingress**: one slow response while the workload starts, then a normal one.
- **The client's source IP**: the woken pod sees the doorman's IP for held connections, not the caller's. Only the connections that arrive while the workload is paused or waking go through the doorman.
- **Right after the pause**: for a second or two, until kube-proxy and ingress controllers pick up the doorman's EndpointSlice, a request can be refused or get a 502 from an ingress, as after any scale-down. A retry is held.
- **A connection held past `maxWait`**: it's closed with no response, which most clients report as an empty reply or a connection reset. A `RequestNotServed` warning event on the ManagedWorkload names the Service and how long the request waited; while the wake is slow, it's emitted at most once a minute.

Track wakes with `hybernate_doorman_wakes_total` and `hybernate_doorman_wait_seconds`; see [Metrics](../reference/metrics.md#doorman). The `HybernateDoormanWakesFailing` alert fires when held connections keep timing out or failing for a workload.
