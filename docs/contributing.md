# Contributing

## Development Setup

### Prerequisites

- Go 1.25+
- Docker
- kubectl
- [Kind](https://kind.sigs.k8s.io/) (for local testing)
- golangci-lint

### Clone and Build

```bash
git clone https://github.com/okedeji/hybernate.git
cd hybernate
make build
```

### Run Tests

```bash
# Unit tests (uses envtest for K8s API)
make test

# Lint
make lint

# E2E tests (requires Kind cluster)
make test-e2e
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
internal/policy/               # Idle and scale policy
internal/signal/               # Signal interface and implementations
internal/lifecycle/            # Pause, resume, scale, destroy
internal/discovery/            # Workload scanning and classification
internal/cost/                 # Cost accumulation
internal/metrics/              # Prometheus metrics
internal/export/               # YAML generation
config/                        # CRD, RBAC, deployment manifests
```

## Code Standards

See [CLAUDE.md](.claude/CLAUDE.md) for the full coding standards. Key points:

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
- [ ] CRD manifests regenerated if types changed (`make manifests`)
- [ ] DeepCopy regenerated if types changed (`make generate`)
- [ ] Documentation updated if user-facing behavior changed

## Regenerating Generated Code

After changing CRD types in `api/v1alpha1/`:

```bash
make generate   # DeepCopy methods
make manifests  # CRD YAML, RBAC
```

Commit generated code separately: `chore(crd): regenerate CRD manifests`
