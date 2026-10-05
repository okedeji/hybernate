# Compatibility

Hybernate supports Kubernetes 1.30 and later, and is tested in CI on 1.30 and 1.37.

[Wake on request](../concepts/wake-on-request.md) adds an EndpointSlice to each Service of a paused workload, one per IP family, pointing at the doorman. Anything that finds a Service's backends through its EndpointSlices sends traffic to the doorman without changes. Each project reads them a little differently, so this page records what's been checked.

**Tested** means an end-to-end test runs it on every change. **Tried by hand** means it was run once on a test cluster for the release listed. **Checked against source** means the project's endpoint code was read for the release listed, but nothing runs it yet.

## Service proxies

| Proxy | Status | Notes |
|-------|--------|-------|
| kube-proxy, kind's default mode | Tested | ClusterIP Services, called from pods in the cluster and through ingress-nginx |
| kube-proxy, other modes (ipvs, nftables), and NodePort and LoadBalancer Services | Not tested yet | kube-proxy programs these from the same EndpointSlices as ClusterIP, but no test runs them |
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
| HTTPS health checks on the traffic port, any provider | **Partial** | The doorman can't read an encrypted health check, so it can't tell it from a request, and each check wakes the workload: it never stays paused longer than the check interval. AWS target groups that use HTTPS, and Google Cloud backend services that use HTTPS or HTTP/2, check over HTTPS by default. Check over HTTP, or on a port of its own listed in `hybernate.io/doorman-ignore-ports`; see [Health checks the doorman can't recognise](../concepts/wake-on-request.md#health-checks-the-doorman-cant-recognise) |
| GKE container-native load balancing (NEGs) | **Not supported** | Services with the `cloud.google.com/neg` annotation aren't routed: the NEG controller rejects endpoints whose pods are in another namespace, and can stop syncing the load balancer altogether. A warning event names them, and the workload reports `WakeOnRequest=False`, reason `UnsupportedLoadBalancer`, when no other Service of its is routed. GKE adds the annotation to Services used by an Ingress by default |

## Service meshes

| Mesh | Status | Notes |
|------|--------|-------|
| Linkerd (edge-26.9) | Checked against source | Connections to the doorman are plain TCP, without mTLS. Services with `internalTrafficPolicy: Local` aren't reached |
| Istio sidecar mode, PERMISSIVE mTLS (v1.31) | Tried by hand | Istio's default. Callers in the mesh reach the doorman in plain text, and the doorman passes the connection to the woken pod, whose sidecar accepts it |
| Istio sidecar mode, STRICT mTLS (v1.31) | **Partial** | The request wakes the workload, but the woken pod's sidecar refuses the doorman's plain-text connection, so the caller gets a 503. Requests once it's Running succeed |
| Istio ambient mode (v1.31) | **Not supported** | ztunnel builds a Service's backends from its pods, not its EndpointSlices, so callers in the mesh never reach the doorman and the workload doesn't wake. Callers outside the mesh still do |
| Istio AuthorizationPolicies that match on the caller's identity | **Not supported** (not tried) | Connections from the doorman carry no caller identity, so a policy that allows only certain callers denies them |

If you run something that isn't listed, or hit a problem with something that is, please [open an issue](https://github.com/okedeji/hybernate/issues). To turn wake on request off for one workload, set `wake.onRequest: false`; for the whole cluster, set the Helm value `doorman.enabled: false`.
