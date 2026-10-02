# Compatibility

[Wake on request](../concepts/wake-on-request.md) adds an EndpointSlice to each Service of a paused workload, pointing at the doorman. Anything that finds a Service's backends through its EndpointSlices sends traffic to the doorman without changes. Each project reads them a little differently, so this page records what's been checked.

**Tested** means an end-to-end test runs it on every change. **Checked against source** means the project's endpoint code was read for the release listed, but nothing runs it yet.

## Service proxies

| Proxy | Status | Notes |
|-------|--------|-------|
| kube-proxy (iptables, ipvs, nftables) | Tested | Covers ClusterIP, NodePort, and LoadBalancer Services |
| Cilium kube-proxy replacement (v1.20) | Checked against source | |

## Ingress controllers and Gateway API

| Controller | Status | Notes |
|------------|--------|-------|
| ingress-nginx (v1.15) | Tested | nginx picks up endpoint changes on a timer, so for a second or two after a pause it can still send a request to the removed pod. It also closes upstream requests after 60 seconds by default; see [How long a connection is held](../concepts/wake-on-request.md#how-long-a-connection-is-held) |
| Traefik (v3.7): Ingress, IngressRoute, Gateway API | Checked against source | |
| Envoy Gateway (v1.9) | Checked against source | |
| Contour (v1.33) | Checked against source | |
| Kong Ingress Controller (v3.5) | Checked against source | |
| HAProxy Kubernetes Ingress (v3.2) | Checked against source | |
| Cilium Ingress and Gateway API (v1.20) | Checked against source | |

## Cloud load balancers

| Load balancer | Status | Notes |
|---------------|--------|-------|
| AWS Load Balancer Controller, target type `instance` | Checked against source | Traffic reaches the doorman through the NodePort |
| AWS Load Balancer Controller, target type `ip` | Checked against source | Registers the doorman pods as targets. The controller must watch the Hybernate namespace, which it does unless `--watch-namespace` is set |
| GKE Ingress without NEGs | Checked against source | Traffic reaches the doorman through the NodePort |
| GKE container-native load balancing (NEGs) | **Not supported** | Services with the `cloud.google.com/neg` annotation aren't routed: the NEG controller rejects endpoints whose pods are in another namespace, and can stop syncing the load balancer altogether. The workload reports `WakeOnRequest=False`, reason `UnsupportedLoadBalancer`, with a warning event. GKE adds the annotation to Services used by an Ingress by default |

## Service meshes

| Mesh | Status | Notes |
|------|--------|-------|
| Linkerd (edge-26.9) | Checked against source | Connections to the doorman are plain TCP, without mTLS. Services with `internalTrafficPolicy: Local` aren't reached |
| Istio (sidecar and ambient) | **Not supported yet** | Planned |

If you run something that isn't listed, or hit a problem with something that is, please [open an issue](https://github.com/okedeji/hybernate/issues). To turn wake on request off for one workload, set `wake.onRequest: false`; for the whole cluster, set the Helm value `doorman.enabled: false`.
