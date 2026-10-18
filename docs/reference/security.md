# Security

What a security review of Hybernate needs: what it can do in a cluster, what it talks to, and how to check that what you install is what was released.

## What it can do

- **It never deletes your resources.** Its one action is to scale a Deployment or StatefulSet to zero, through the scale subresource, and back. The only other change it makes to a workload is its own `hybernate.io/paused-from` annotation while the workload is paused, though its role may patch workloads, and the only things it deletes are its own ManagedWorkloads and EndpointSlices.
- **It never reads Secrets.**
- **It can write any EndpointSlice in the namespaces it works in.** It only ever creates, changes and deletes its own, labelled `endpointslice.kubernetes.io/managed-by: doorman.hybernate.io`, and never overwrites one without that label, but RBAC can't limit a role by label, so its ServiceAccount is allowed the rest too. Whoever holds its token could redirect any Service there.
- **It can be confined to namespaces.** With [`watchNamespaces`](helm-values.md#namespaces), the operator and doorman get a Role in each of those namespaces instead of a ClusterRole, so Kubernetes itself refuses them anything elsewhere. The operator keeps one ClusterRole, read-only, on Namespaces and Nodes, and a Role in its own namespace to read the doorman's EndpointSlices. With secure metrics (the default), both also keep a ClusterRoleBinding to `system:auth-delegator`, which only lets them check the tokens of whoever scrapes their metrics.
- **It can be kept out of namespaces.** A namespace labelled `hybernate.io/protected=true`, or matching [`protectedNamespaces`](helm-values.md#namespaces), is never managed; see [Protected namespaces](../guides/opt-in.md#protected-namespaces).
- **A namespace's workloads are only kept awake for another namespace that it allows.** A dependency in another namespace, declared in `dependsOn` or learned, is held awake, woken and waited for only when its namespace's `hybernate.io/dependents-from` annotation lists the dependent's namespace. Otherwise it's ignored and reported as `DependencyNotAllowed` with the same message whether it exists or not, so a dependency can't probe another namespace. A dependency learned from a request that woke a workload is only learned from a `hybernate.io/last-request-from` the doorman wrote, by the field manager the API server records, and only for a sender in a namespace the woken workload's namespace allows. See [Across namespaces](../concepts/dependencies.md#across-namespaces).
- **It doesn't fight people or tools.** A paused workload scaled up by anything else wakes; a GitOps tool undoing a pause is reported, not fought; see [GitOps tools](../guides/gitops.md).

Everything it reads, writes, and sends is listed in [Data and Access](data-and-access.md), and the exact RBAC in [Helm values](helm-values.md#rbac).

## What it talks to

| From | To | Why |
|---|---|---|
| Operator, doorman | The Kubernetes API server | Everything it does |
| Operator | Prometheus, at `prometheus.url` | Only with Prometheus activity queries configured |
| Any client of a paused workload's Service: a pod, an ingress controller, or a load balancer through a NodePort | Doorman | The doorman holds the connection, wakes the workload, and forwards it |
| Doorman | The woken workload's pods, in any routed namespace, on their Service ports | Passing the held connection through |
| Prometheus | Operator and doorman metrics | Only with a token allowed to get `/metrics`, unless `metrics.secure` is off |

Nothing leaves the cluster: no telemetry, no usage reporting, no update checks. The images run as a non-root user on a distroless base, with a read-only root filesystem. So do the chart's guards, hook Jobs that run only during `helm uninstall`, `helm upgrade` and `helm rollback`, from the operator's own image, each with a ServiceAccount that may only list ManagedWorkloads.

### The doorman is a way in to paused workloads

Any client that can reach a doorman pod can reach every routed, paused workload, in any namespace, on its Service ports, and a workload's own NetworkPolicy sees the doorman, not the client, as the source. The doorman listens on all of its pod's interfaces: binding to the pod IP would only shut out containers in its own pod, so a NetworkPolicy on the doorman is the control that matters. The chart's NetworkPolicy (`networkPolicy.enabled`, off by default) leaves the doorman's routing ports open to every source unless you restrict them. On any cluster where untrusted pods share the network, enable `networkPolicy` and set `doorman.networkPolicy.ingressFrom`. Where the default is too wide:

- Admit only the callers your paused workloads really have, such as your ingress controller's namespace and the namespaces whose apps call paused services, with `doorman.networkPolicy.ingressFrom`. See [Wake on Request](../concepts/wake-on-request.md#restricting-who-can-reach-the-doorman).
- Give them an egress NetworkPolicy allowing only your pod CIDRs and the API server: set `doorman.networkPolicy.egress` (off by default; see [Helm values](helm-values.md#network-policy)). The doorman only dials Pod-backed IP endpoints of EndpointSlices the EndpointSlice controller manages, and refuses loopback, link-local, and cloud metadata addresses (AWS's `fd00:ec2::254` and Alibaba Cloud's `100.100.100.200` included). Anyone who can write EndpointSlices in a namespace can forge those labels, so the doorman also reads the pod each endpoint names and dials the address only if that pod is in the Service's namespace and has it: a forged slice can send it only to that namespace's own pods, never past a NetworkPolicy into another namespace. The operator checks the same way before it leaves a Service to pods it says are already serving it, so a forged slice can't keep a paused workload's Service from waking it either. The policy enforces the same bounds in the network.
- Its rate limits (see [Limits](../concepts/wake-on-request.md#limits)) slow a port scan through the doorman; they don't stop one. One workload can hold at most 256 of a doorman pod's 2048 places, so a workload that never becomes Ready can't stop other workloads' requests from being held.

The doorman's ServiceAccount can patch ManagedWorkloads, to set `hybernate.io/last-request` and `hybernate.io/last-request-from`, the caller's IP address. The chart's `<fullname>-doorman` ValidatingAdmissionPolicy (`doorman.admissionPolicy.enabled`, on by default) holds it to those two annotations: the API server refuses a patch from it that changes a ManagedWorkload's spec, labels, finalizers, owners or any other annotation. The kustomize manifests include it too, and it follows the `namespace` and `namePrefix` set in `config/default`; an overlay on top of `config/default` that changes either must patch the policy's `matchConditions` to match.

## Verifying a release

Every release is built by the [release workflow](https://github.com/okedeji/hybernate/blob/main/.github/workflows/release.yml) from a tag, and signed keylessly with [Sigstore](https://www.sigstore.dev/): the signature is bound to that workflow and tag, so there's no key to steal. Each command below fails unless the artifact came from that workflow. The signatures are made with cosign 3, so verify them with cosign 3 or later.

**The image**, its signature, an SBOM for each platform, and build provenance:

```bash
IMAGE=ghcr.io/okedeji/hybernate:v0.2.0
cosign verify $IMAGE \
  --certificate-identity-regexp '^https://github.com/okedeji/hybernate/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify-attestation $IMAGE --type spdxjson \
  --certificate-identity-regexp '^https://github.com/okedeji/hybernate/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://$IMAGE --repo okedeji/hybernate
```

**The Helm chart:**

```bash
cosign verify ghcr.io/okedeji/charts/hybernate:0.2.0 \
  --certificate-identity-regexp '^https://github.com/okedeji/hybernate/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

**The plugin, `install.yaml`, and the other release files**, through their signed checksums:

```bash
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/okedeji/hybernate/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --check --ignore-missing checksums.txt
gh attestation verify kubectl-hybernate-linux-amd64.tar.gz --repo okedeji/hybernate
```

## Vulnerabilities

- **Every pull request** runs [govulncheck](https://go.dev/doc/security/vuln/) on the Go code, which fails on any known vulnerability the code can reach, and [Trivy](https://trivy.dev/) on the built image, which fails on critical and high vulnerabilities that have a fix. A vulnerability with no fix yet is reported, not blocking.
- **Every release** runs both again before anything is published, and attaches Trivy's full report for each platform to the GitHub release as `vulnerabilities-linux-amd64.txt` and `vulnerabilities-linux-arm64.txt`, alongside the SBOMs, `sbom-linux-amd64.spdx.json` and `sbom-linux-arm64.spdx.json`.
- **Every day**, both run against `main`, and Trivy against the released image, `ghcr.io/okedeji/hybernate:latest`, for both platforms, so a vulnerability disclosed after a release is reported then rather than at the next pull request.
- **Dependencies** (Go modules, GitHub Actions, base images) are kept current by Dependabot, weekly. Every Action is pinned to a commit, and the base images to a digest.

To report a vulnerability, see [SECURITY.md](https://github.com/okedeji/hybernate/blob/main/SECURITY.md): privately, through a GitHub security advisory.
