# Contributing

## Development Setup

### Prerequisites

- Go 1.26+ (`go.mod` pins the toolchain)
- Docker (`make test-alerts` runs promtool in a container, so you don't need it installed)
- kubectl
- [Kind](https://kind.sigs.k8s.io/) (for the end-to-end and Helm tests)
- Helm, only for `make check-chart` and `make test-alerts`

`make` downloads controller-gen, kustomize, setup-envtest and golangci-lint into `bin/` as it needs them.

### Clone and Build

```bash
git clone https://github.com/okedeji/hybernate.git
cd hybernate
make build
```

### Run Tests

```bash
# Unit and integration tests (uses envtest for the K8s API)
make test

# Lint
make lint

# E2E tests: creates a Kind cluster, runs, and deletes it (KIND_NODE_IMAGE picks the Kubernetes version)
make test-e2e

# Install the Helm chart into Kind, and pause and wake a workload through the doorman
make test-helm-smoke
```

### Run Locally

```bash
# Install CRDs into your current cluster
make install

# Run the operator locally (outside the cluster)
make run
```

### Build the kubectl Plugin

```bash
make build-plugin
# Binary at bin/kubectl-hybernate
```

## Project Structure

```
cmd/main.go                    # Operator entrypoint
cmd/kubectl-hybernate/main.go  # kubectl plugin
api/v1alpha1/                  # CRD type definitions
internal/controller/           # Reconcilers
internal/forecast/             # Holt-Winters engine
internal/signal/               # Prometheus activity queries
internal/lifecycle/            # Pause and resume
internal/autoscaler/           # HPA and KEDA
internal/gitops/               # GitOps tool detection
internal/discovery/            # Cluster scan for kubectl hybernate scan
internal/doorman/              # Wake on request
internal/cost/                 # Cost rates and node list prices
internal/metrics/              # Prometheus metrics, and the Metrics API reader
charts/hybernate/              # Helm chart
config/                        # CRD, RBAC, kustomize manifests
test/e2e/                      # End-to-end tests, in Kind
test/docs/                     # Validates the docs' ManagedWorkloads against the CRD, and Helm values against the chart
tests/prometheus/              # promtool tests for the alert rules
docs/                          # This site (mkdocs)
```

## Code Standards

Key coding standards:

- **No comments that restate what the code does.** Comments are for *why*, not *what*.
- **Wrap errors with context:** `fmt.Errorf("doing X: %w", err)`
- **Return early on errors.** Happy path flows straight down.
- **Accept interfaces, return structs.** Define interfaces at the point of consumption.
- **Test behavior, not implementation.** Table-driven tests with `testify`.

## Commit Conventions

Use [Conventional Commits](https://www.conventionalcommits.org/):

```
feat(idle): add idle detection with detector and webhook signal
fix(idle): reset idle timer on active signal
refactor(forecast): extract confidence scoring into separate type
chore(crd): regenerate CRD manifests
docs(guides): add Prometheus signals guide
test(policy): add table tests for scale constraints
```

One logical change per commit. Generated code gets its own commit.

## Pull Requests

1. Fork the repository
2. Create a feature branch from `main`
3. Make your changes with tests
4. Run `make lint test` and ensure everything passes
5. Open a PR with a clear description of what and why

### PR Checklist

- [ ] Tests pass (`make test`)
- [ ] Lint passes (`make lint`)
- [ ] CRD manifests regenerated if types changed (`make manifests`), and generated code is current (`make verify-generated`)
- [ ] DeepCopy regenerated if types changed (`make generate`)
- [ ] The chart still lints and renders, with RBAC matching `config/rbac` (`make check-chart`), if you changed it or RBAC
- [ ] Alert rules still pass their tests (`make test-alerts`), if you changed them
- [ ] Documentation updated if user-facing behavior changed

CI also runs the end-to-end tests on Kubernetes 1.30 and 1.37, the Helm smoke test, and `make vulncheck`.

## Documentation

The docs are built with mkdocs: `pip install -r docs/requirements.txt`, then `mkdocs serve`. `make test` creates every ManagedWorkload shown in a `yaml` block in `README.md` and `docs/`, and in `config/samples/`, against the real CRD, so a doc example that doesn't validate fails the build. A partial one, a `spec:` block or spec fields such as `dependsOn` on their own, is wrapped into a whole ManagedWorkload first; title it `managedworkload.yaml` or leave it untitled, since a block titled as another file, such as `application.yaml`, isn't read as one. Helm values blocks, titled `values.yaml` or `values-<name>.yaml` or untitled with only the chart's keys, are checked against `charts/hybernate/values.schema.json`, and every value they set must be one the chart has. Put `<!-- snippet -->` on the line before a block that's deliberately partial and shouldn't be checked. Links to `https://okedeji.io/hybernate` in the code must point at a page in `docs/`.

## Releasing

A release is a tag on `main`, `vX.Y.Z`. `make release VERSION=X.Y.Z` bumps the chart's version and appVersion, the kustomize image, and the install and verify commands in the docs, dates the version's section of `CHANGELOG.md`, commits that, and tags it. Pushing the tag starts the release workflow:

1. Checks the chart, the kustomize image, the docs and the dated changelog name the tag's version (`make verify-version`), then runs every check a pull request does, including Trivy on the image.
2. Builds the plugin archives, the krew manifest, `install.yaml` and the chart; pushes the image by digest, untagged, and signs it and attaches its SBOMs and provenance; and drafts the GitHub release with every file, its signed checksums, and the changelog section as its notes.
3. Tags the image, pushes and signs the chart, tags the image `latest`, and publishes the GitHub release, which moves `releases/latest` and then the docs site.

Until step 3 nothing users install has changed, so a run that fails earlier can be run again: it replaces its draft. One that fails in step 3 can be too.

**Rehearse a release with a release candidate first.** A tag with a hyphen, such as `v0.2.0-rc.1`, runs the whole workflow as a pre-release: everything is published but `latest`, so the image, the chart and the GitHub release exist only for those who ask for that version, krew and the install docs' `releases/latest` links stay on the last release, and the docs site isn't rebuilt. Its notes are its version's changelog section, which needn't be dated yet. From a throwaway branch, so `main` keeps the release's version:

```bash
git switch -c rehearse-v0.2.0-rc.1 main
make release VERSION=0.2.0-rc.1
git push origin v0.2.0-rc.1
```

Once it succeeds, install the release candidate as the docs describe, with `--version 0.2.0-rc.1`, and check its signatures with the commands in [Security](reference/security.md#verifying-a-release). Then release from `main`:

```bash
git switch main
make release VERSION=0.2.0
git push origin main v0.2.0
```

## Regenerating Generated Code

After changing CRD types in `api/v1alpha1/`:

```bash
make generate   # DeepCopy methods
make manifests  # CRD YAML, RBAC
```

Commit generated code separately: `chore(crd): regenerate CRD manifests`
