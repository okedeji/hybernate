# Image URL to use all building/pushing image targets
IMG ?= controller:latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	./hack/chart-crds.sh config/crd/bases charts/hybernate/templates/crds

.PHONY: prices
prices: ## Regenerate the on-demand list prices nodes are priced at, before each release
	go run ./hack/prices internal/cost/prices.csv.gz

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# The e2e setup assumes Kind is installed, and builds and loads the manager
# image itself. KIND_NODE_IMAGE picks the Kubernetes version.
KIND_CLUSTER ?= hybernate-test-e2e
KIND_NODE_IMAGE ?=

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) $(if $(KIND_NODE_IMAGE),--image $(KIND_NODE_IMAGE)) ;; \
	esac

E2E_IMAGES ?= curlimages/curl:8.7.1 registry.k8s.io/pause:3.10 registry.k8s.io/metrics-server/metrics-server:v0.7.2 \
	registry.k8s.io/e2e-test-images/agnhost:2.52 registry.k8s.io/ingress-nginx/controller:v1.15.1 \
	registry.k8s.io/ingress-nginx/kube-webhook-certgen:v1.6.9 ghcr.io/kedacore/keda:2.20.2 \
	ghcr.io/kedacore/keda-metrics-apiserver:2.20.2 ghcr.io/kedacore/keda-admission-webhooks:2.20.2
E2E_PROCS ?= 4
E2E_PLATFORM ?= linux/$(shell go env GOARCH)

# Streams each image into the Kind node's containerd for one platform only.
# `kind load docker-image` imports all platforms, which fails under Docker
# Desktop's containerd image store: a pull only fetches the host platform,
# so the other platforms' content is missing.
.PHONY: load-test-e2e-images
load-test-e2e-images: ## Preload the images the e2e specs run into the Kind cluster
	@for img in $(E2E_IMAGES); do \
		echo "Loading $$img ($(E2E_PLATFORM)) into $(KIND_CLUSTER)"; \
		docker pull --quiet --platform $(E2E_PLATFORM) $$img >/dev/null && \
		docker save $$img | docker exec -i $(KIND_CLUSTER)-control-plane \
			ctr --namespace=k8s.io images import --platform $(E2E_PLATFORM) --snapshotter=overlayfs - >/dev/null \
		|| exit 1; \
	done

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	$(MAKE) load-test-e2e-images
	@# The specs wait on real idle clocks, so their containers run in
	@# parallel against the one cluster, through the Ginkgo CLI, which go
	@# test can't do.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go run github.com/onsi/ginkgo/v2/ginkgo -p --procs=$(E2E_PROCS) \
		--tags=e2e --timeout=40m -v ./test/e2e/
	$(MAKE) cleanup-test-e2e

.PHONY: test-helm-smoke
test-helm-smoke: ## Install the Helm chart with watchNamespaces into Kind, and pause and wake a workload through the doorman
	KIND=$(KIND) ./hack/helm-smoke.sh

PROMETHEUS_IMAGE ?= prom/prometheus:v3.15.0@sha256:efd719c99d83b060d9daefdcf00360461adf279f45ef5391f8d111892118753e

.PHONY: test-alerts
test-alerts: ## Unit-test the chart's and config/prometheus's alert rules with promtool
	@dir=$$(mktemp -d); trap 'rm -rf "$$dir"' EXIT; \
	cp tests/prometheus/*_test.yaml "$$dir"; \
	go run ./hack/promrules < config/prometheus/alerts.yaml > "$$dir/kustomize.rules.yaml"; \
	helm template hybernate charts/hybernate --set metrics.prometheusRule.enabled=true \
		--show-only templates/prometheusrule.yaml | go run ./hack/promrules > "$$dir/chart.rules.yaml"; \
	$(CONTAINER_TOOL) run --rm -v "$$dir:/rules" -w /rules --entrypoint promtool $(PROMETHEUS_IMAGE) \
		test rules $$(cd "$$dir" && ls *_test.yaml)

.PHONY: check-chart
check-chart: ## Lint the Helm chart, render it for each provider's Kubernetes versions, and check its RBAC matches config/rbac
	helm lint charts/hybernate --strict
	@for v in v1.26.0 v1.30.2-eks-1552ad0 v1.31.1-gke.1678000 v1.32.5 v1.34.1; do \
		echo "rendering for Kubernetes $$v"; \
		helm template hybernate charts/hybernate --kube-version $$v >/dev/null || exit 1; \
	done
	@for d in 0s 0.0m 0h0m0s; do \
		! helm template hybernate charts/hybernate --set defaults.idleAfter=$$d >/dev/null 2>&1 || { \
			echo "the chart accepts defaults.idleAfter=$$d, which the operator refuses to start with"; exit 1; }; \
	done
	@for d in 1h30m 0.5s 0h5m; do \
		helm template hybernate charts/hybernate --set defaults.idleAfter=$$d >/dev/null || exit 1; \
	done
	helm template hybernate charts/hybernate | go run ./hack/rbaccheck config/rbac/role.yaml
	helm template hybernate charts/hybernate --set 'watchNamespaces={shop,blog}' | go run ./hack/rbaccheck config/rbac/role.yaml

.PHONY: verify-generated
verify-generated: manifests generate ## Fail if generated code or manifests, or go.mod, aren't up to date
	@git diff --exit-code -- api config charts || { \
		echo "Generated files are out of date; run make manifests generate and commit the result."; exit 1; }
	@test -z "$$(git status --porcelain -- api config charts)" || { \
		git status --porcelain -- api config charts; echo "Generated files are untracked; commit them."; exit 1; }
	go mod tidy -diff

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

GOVULNCHECK_VERSION ?= v1.8.0

.PHONY: vulncheck
vulncheck: ## Report vulnerabilities in Go dependencies that Hybernate's code reaches
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Versioning

# The docs that name the current release, for installing or verifying it.
# docs/operations/upgrading.md isn't one: its versions are history.
VERSIONED_DOCS = README.md docs/getting-started/installation.md docs/reference/helm-values.md docs/reference/security.md

.PHONY: bump
bump: ## Bump the version in the chart, the kustomize image, and docs. Usage: make bump VERSION=0.1.2
	@if [ -z "$(VERSION)" ]; then echo "VERSION is required. Usage: make bump VERSION=0.1.2"; exit 1; fi
	@old=$$(awk '/^version:/ {print $$2}' charts/hybernate/Chart.yaml); \
	OLD="$$old" NEW="$(VERSION)" perl -pi -e 's/(?<![\d.])\Q$$ENV{OLD}\E(?!\.?[\w-])/$$ENV{NEW}/g' $(VERSIONED_DOCS)
	@perl -pi -e 's/^version:.*/version: $(VERSION)/' charts/hybernate/Chart.yaml
	@perl -pi -e 's/^appVersion:.*/appVersion: "v$(VERSION)"/' charts/hybernate/Chart.yaml
	@perl -pi -e 's/^  newTag:.*/  newTag: v$(VERSION)/' config/manager/kustomization.yaml
	@echo "Bumped to $(VERSION)"

.PHONY: release
release: bump ## Bump version, commit, and tag. Usage: make release VERSION=0.1.2
	@git add charts/hybernate/Chart.yaml config/manager/kustomization.yaml $(VERSIONED_DOCS)
	@git commit -m "chore(release): bump version to v$(VERSION)"
	@git tag -a v$(VERSION) -m "v$(VERSION)"
	@echo "Tagged v$(VERSION). Run 'git push origin main v$(VERSION)' to release."

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: build-plugin
build-plugin: fmt vet ## Build kubectl-hybernate plugin binary.
	go build -o bin/kubectl-hybernate ./cmd/kubectl-hybernate

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name hybernate-builder
	$(CONTAINER_TOOL) buildx use hybernate-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm hybernate-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.20.1

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.8.0
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
