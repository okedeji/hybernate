# Security

What a security review of Hybernate needs: what it can do in a cluster, what it talks to, and how to check that what you install is what was released.

## What it can do

- **It never deletes your resources.** Its one action is to scale a Deployment or StatefulSet to zero, through the scale subresource, and back. It can't change any other field of a workload, and the only things it deletes are its own ManagedWorkloads and EndpointSlices.
- **It never reads Secrets.**
- **It can be confined to namespaces.** With [`watchNamespaces`](helm-values.md#namespaces), the operator and doorman get a Role in each of those namespaces instead of a ClusterRole, so Kubernetes itself refuses them anything elsewhere. The operator keeps one ClusterRole, read-only, on Namespaces and Nodes.
- **It can be kept out of namespaces.** A namespace labelled `hybernate.io/protected=true`, or matching [`protectedNamespaces`](helm-values.md#namespaces), is never managed; see [Protected namespaces](../guides/opt-in.md#protected-namespaces).
- **It doesn't fight people or tools.** A paused workload scaled up by anything else wakes; a GitOps tool undoing a pause is reported, not fought; see [Argo CD and Flux](../guides/gitops.md).

Everything it reads, writes, and sends is listed in [Data and Access](data-and-access.md), and the exact RBAC in [Helm values](helm-values.md#rbac).

## What it talks to

| From | To | Why |
|---|---|---|
| Operator, doorman | The Kubernetes API server | Everything it does |
| Operator | Prometheus, at `prometheus.url` | Only with Prometheus activity queries configured |
| Any pod calling a paused workload's Service | Doorman | The doorman holds the connection, wakes the workload, and forwards it |

Nothing leaves the cluster: no telemetry, no usage reporting, no update checks. The images run as a non-root user on a distroless base, with a read-only root filesystem.

## Verifying a release

Every release is built by the [release workflow](https://github.com/okedeji/hybernate/blob/main/.github/workflows/release.yml) from a tag, and signed keylessly with [Sigstore](https://www.sigstore.dev/): the signature is bound to that workflow and tag, so there's no key to steal. Each command below fails unless the artifact came from that workflow.

**The image**, its signature, SBOM, and build provenance:

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
cosign verify ghcr.io/okedeji/charts/hybernate:v0.2.0 \
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
- **Every release** runs both again before anything is pushed, and attaches Trivy's full report to the GitHub release as `vulnerabilities.txt`, alongside the SBOM.
- **Dependencies** (Go modules, GitHub Actions, base images) are kept current by Dependabot, weekly. Every Action is pinned to a commit, and the base images to a digest.

To report a vulnerability, see [SECURITY.md](https://github.com/okedeji/hybernate/blob/main/SECURITY.md): privately, through a GitHub security advisory.
