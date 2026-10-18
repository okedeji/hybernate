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
	go vet -tags e2e ./test/...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test -race $$(go list ./... | grep -v /e2e) -coverprofile cover.out

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
		--tags=e2e --timeout=60m -v ./test/e2e/
	$(MAKE) cleanup-test-e2e

.PHONY: test-helm-smoke
test-helm-smoke: ## Install the Helm chart with watchNamespaces into Kind, and pause and wake a workload through the doorman
	KIND=$(KIND) ./hack/helm-smoke.sh

PROMETHEUS_IMAGE ?= prom/prometheus:v3.15.0@sha256:efd719c99d83b060d9daefdcf00360461adf279f45ef5391f8d111892118753e

.PHONY: test-alerts
test-alerts: ## Unit-test the chart's and config/prometheus's alert rules with promtool
	@# The tests' series carry the workload's namespace, as a scrape keeps it
	@# only when every ServiceMonitor endpoint honors the target's labels.
	@test "$$(helm template hybernate charts/hybernate --set metrics.serviceMonitor.enabled=true \
		--show-only templates/servicemonitor.yaml | grep -c 'honorLabels: true')" = 2 || { \
		echo "the chart's ServiceMonitors don't both set honorLabels, so the workload's namespace label is overwritten"; exit 1; }
	@test "$$(grep -c 'honorLabels: true' config/prometheus/monitor.yaml)" = 2 || { \
		echo "config/prometheus's ServiceMonitors don't both set honorLabels, so the workload's namespace label is overwritten"; exit 1; }
	@dir=$$(mktemp -d); trap 'rm -rf "$$dir"' EXIT; \
	cp tests/prometheus/*_test.yaml "$$dir"; \
	go run ./hack/promrules < config/prometheus/alerts.yaml > "$$dir/kustomize.rules.yaml"; \
	helm template hybernate charts/hybernate --set metrics.prometheusRule.enabled=true --set metrics.serviceMonitor.enabled=true \
		--show-only templates/prometheusrule.yaml | go run ./hack/promrules > "$$dir/chart.rules.yaml"; \
	$(CONTAINER_TOOL) run --rm --user "$$(id -u):$$(id -g)" -v "$$dir:/rules" -w /rules --entrypoint promtool $(PROMETHEUS_IMAGE) \
		test rules $$(cd "$$dir" && ls *_test.yaml)

.PHONY: check-chart
check-chart: ## Lint the Helm chart, render it for each provider's Kubernetes versions, and check its RBAC matches config/rbac
	helm lint charts/hybernate --strict
	@for v in v1.30.0 v1.30.2-eks-1552ad0 v1.31.1-gke.1678000 v1.32.5 v1.34.1 v1.37.0; do \
		echo "rendering for Kubernetes $$v"; \
		helm template hybernate charts/hybernate --kube-version $$v >/dev/null || exit 1; \
	done
	@! helm template hybernate charts/hybernate --kube-version v1.29.9 >/dev/null 2>&1 || { \
		echo "the chart installs on Kubernetes 1.29, below the 1.30 it supports"; exit 1; }
	@for d in 0s 0.0m 0h0m0s 59s 0.5m 500ms; do \
		! helm template hybernate charts/hybernate --set defaults.idleAfter=$$d >/dev/null 2>&1 || { \
			echo "the chart accepts defaults.idleAfter=$$d, under a minute, which the operator refuses to start with"; exit 1; }; \
	done
	@! helm template hybernate charts/hybernate --set 'metrics.readerSubjects[0].kind=ServiceAccount' \
		--set 'metrics.readerSubjects[0].name=prometheus' >/dev/null 2>&1 || { \
		echo "the chart accepts a ServiceAccount in metrics.readerSubjects without its namespace, which the API server refuses"; exit 1; }
	@for v in 'watchNamespace={staging}' doorman.enabeld=false metrics.serviceMonitor.enable=true; do \
		! helm template hybernate charts/hybernate --set "$$v" >/dev/null 2>&1 || { \
			echo "the chart accepts $$v, a value it doesn't have, and would ignore it"; exit 1; }; \
	done
	@for d in 1h30m 1m 90s 0h5m 1.5h; do \
		helm template hybernate charts/hybernate --set defaults.idleAfter=$$d >/dev/null || exit 1; \
	done
	@helm template hybernate charts/hybernate --set networkPolicy.enabled=true \
		--set 'doorman.networkPolicy.ingressFrom[0].namespaceSelector.matchLabels.team=checkout' \
		--show-only templates/networkpolicy.yaml | grep -q 'team: checkout' || { \
		echo "doorman.networkPolicy.ingressFrom doesn't reach the doorman's NetworkPolicy"; exit 1; }
	@! helm template hybernate charts/hybernate \
		--set 'doorman.networkPolicy.ingressFrom[0].namespaceSelector.matchLabels.team=checkout' >/dev/null 2>&1 || { \
		echo "the chart accepts doorman.networkPolicy.ingressFrom without networkPolicy.enabled, which leaves the doorman open"; exit 1; }
	helm template hybernate charts/hybernate | go run ./hack/rbaccheck config/rbac/role.yaml
	helm template hybernate charts/hybernate --set 'watchNamespaces={shop,blog}' | go run ./hack/rbaccheck config/rbac/role.yaml
	@long=abcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabc; \
	names=$$(helm template hybernate charts/hybernate --set fullnameOverride=$$long --set networkPolicy.enabled=true \
		--set metrics.serviceMonitor.enabled=true --set metrics.prometheusRule.enabled=true | \
		awk '/^kind:/ {k = $$2} /^[a-zA-Z]/ {m = ($$1 == "metadata:")} m && /^  name: / {print k, $$2}'); \
	! awk 'length($$2) > 63 {print "too long: " $$0; bad = 1} END {exit !bad}' <<<"$$names" && \
		[ -z "$$(sort <<<"$$names" | uniq -d)" ] || { \
		echo "a 63-character fullnameOverride makes a name over 63 characters, or two resources the same name"; exit 1; }
	@crd=$$(helm template hybernate charts/hybernate --show-only templates/crds/hybernate.io_managedworkloads.yaml); \
	grep -q 'helm.sh/resource-policy: keep' <<<"$$crd" && grep -q 'argocd.argoproj.io/sync-options: Delete=false' <<<"$$crd" || { \
		echo "the chart's CRD isn't kept on uninstall by both Helm and Argo CD, which would delete every ManagedWorkload"; exit 1; }
	@for p in 8081 20000 29999; do \
		! helm template hybernate charts/hybernate --set metrics.port=$$p >/dev/null 2>&1 || { \
			echo "the chart accepts metrics.port=$$p, which the health probes or the doorman's routing ports use"; exit 1; }; \
	done
	@helm template hybernate charts/hybernate --set metrics.port=30000 >/dev/null
	@out=$$(helm template hybernate charts/hybernate --set-string podLabels.team=checkout \
		--set-string podLabels.control-plane=other --set priorityClassName=hybernate-critical \
		--set 'doorman.extraEnv[0].name=GOMAXPROCS' --set-string 'doorman.extraEnv[0].value=2'); \
	[ "$$(grep -c 'team: checkout' <<<"$$out")" = 2 ] && [ "$$(grep -c 'priorityClassName: hybernate-critical' <<<"$$out")" = 3 ] && \
		grep -q 'name: GOMAXPROCS' <<<"$$out" && ! grep -q 'control-plane: other' <<<"$$out" || { \
		echo "podLabels, priorityClassName or doorman.extraEnv don't reach the operator's, the doorman's and the guard's pods, or podLabels replaced a selector label"; exit 1; }
	@out=$$(helm template hybernate charts/hybernate --set 'doorman.healthCheckUserAgents={Bot #1,Probe: v2}' \
		--set 'protectedNamespaces={*-prod}' --set timezone=Europe/London); \
	grep -qF -- '- "--doorman-health-check-user-agents=Bot #1,Probe: v2"' <<<"$$out" && \
		grep -qF -- '- "--protected-namespaces=*-prod"' <<<"$$out" && grep -qF -- '- "--timezone=Europe/London"' <<<"$$out" || { \
		echo "the chart doesn't quote a container argument, so a # starts a YAML comment, or a ': ' makes it a map"; exit 1; }
	@! helm template hybernate charts/hybernate | grep -q -- '--guard=upgrade' && \
		helm template hybernate charts/hybernate --set 'watchNamespaces={shop}' | grep -q -- '--guard=upgrade' || { \
		echo "the upgrade guard doesn't run exactly when watchNamespaces is set, so narrowing it can strand paused workloads"; exit 1; }
	@out=$$(helm template hybernate charts/hybernate --show-only templates/guard.yaml --set-string guard.podAnnotations.team=checkout \
		--set-string guard.podLabels.control-plane=other --set 'tolerations[0].operator=Exists'); \
	grep -q 'sidecar.istio.io/inject: "false"' <<<"$$out" && grep -q 'linkerd.io/inject: disabled' <<<"$$out" && \
		grep -q 'team: checkout' <<<"$$out" && grep -q 'operator: Exists' <<<"$$out" && \
		! grep -q 'control-plane: other' <<<"$$out" && ! grep -q 'runAsUser' <<<"$$out" || { \
		echo "the guard's pod lets a mesh's sidecar in, which never exits, ignores guard.* or the operator's scheduling, or fixes a UID OpenShift refuses"; exit 1; }
	@for field in terminationGracePeriodSeconds: seconds: minReadySeconds:; do \
		chart=$$(helm template hybernate charts/hybernate --show-only templates/doorman.yaml | awk -v f="$$field" '$$1 == f {print $$2}'); \
		kustomize=$$(awk -v f="$$field" '$$1 == f {print $$2}' config/manager/doorman.yaml); \
		[ -n "$$chart" ] && [ "$$chart" = "$$kustomize" ] || { \
			echo "config/manager's doorman has $$field '$$kustomize', the chart's '$$chart', so one drops connections on shutdown"; exit 1; }; \
	done
	@chart=$$(helm template hybernate charts/hybernate --show-only templates/deployment.yaml | awk '$$1 == "terminationGracePeriodSeconds:" {print $$2}'); \
	kustomize=$$(awk '$$1 == "terminationGracePeriodSeconds:" {print $$2}' config/manager/manager.yaml); \
	[ "$$chart" -gt 30 ] && [ "$$chart" = "$$kustomize" ] || { \
		echo "the operator's grace period, the chart's '$$chart' and config/manager's '$$kustomize', isn't over the 30s its reconciles get to finish"; exit 1; }
	@! helm template hybernate charts/hybernate --set metrics.prometheusRule.enabled=true | grep -q 'alert: Hybernate.*Down' && \
		[ "$$(helm template hybernate charts/hybernate --set metrics.prometheusRule.enabled=true --set metrics.serviceMonitor.enabled=true | \
			grep -c 'alert: Hybernate.*Down')" = 2 ] || { \
		echo "HybernateDown and HybernateDoormanDown aren't created exactly with the ServiceMonitors whose jobs they match, so they fire forever"; exit 1; }
	@! helm template hybernate charts/hybernate --set metrics.enabled=false | grep -q 'system:auth-delegator' || { \
		echo "the chart lets the operator and the doorman review tokens with metrics disabled, when nothing checks one"; exit 1; }
	@[ "$$(helm template hybernate charts/hybernate --show-only templates/user-roles.yaml | grep -c 'aggregate-to-view: "true"')" = 1 ] && \
		grep -q 'aggregate-to-view: "true"' config/rbac/managedworkload_viewer_role.yaml && \
		grep -q 'aggregate-to-edit: "true"' config/rbac/managedworkload_editor_role.yaml || { \
		echo "ManagedWorkloads aren't in the built-in view and edit roles, so namespace users can't see or pause them"; exit 1; }
	@helm template hybernate charts/hybernate --set leaderElection.enabled=false --show-only templates/deployment.yaml | grep -q 'type: Recreate' && \
		! helm template hybernate charts/hybernate --show-only templates/deployment.yaml | grep -q 'type: Recreate' || { \
		echo "without leader election, a rolling update runs two operators at once"; exit 1; }

.PHONY: test-doorman-policy
test-doorman-policy: setup-envtest kustomize ## Check against envtest that the chart's and kustomize's admission policies hold the doorman to stamping requests
	@dir=$$(mktemp -d); trap 'rm -rf "$$dir"' EXIT; \
	helm template hybernate charts/hybernate --namespace hybernate-system > "$$dir/chart.yaml"; \
	cp -R config "$$dir/config"; \
	sed -e 's/^namespace: hybernate-system$$/namespace: staging-system/' -e 's/^namePrefix: hybernate-$$/namePrefix: staging-/' \
		config/default/kustomization.yaml > "$$dir/config/default/kustomization.yaml"; \
	"$(KUSTOMIZE)" build "$$dir/config/default" > "$$dir/kustomize.yaml"; \
	grep -q 'namePrefix: staging-' "$$dir/config/default/kustomization.yaml" || { \
		echo "couldn't rename the kustomize install to check its admission policy follows"; exit 1; }; \
	CHART_MANIFEST="$$dir/chart.yaml" KUSTOMIZE_MANIFEST="$$dir/kustomize.yaml" \
	KUBEBUILDER_ASSETS="$$("$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		go test -count=1 ./hack/doormanpolicy

.PHONY: verify-generated
verify-generated: manifests generate verify-go-version ## Fail if generated code or manifests, or go.mod, aren't up to date
	@git diff --exit-code -- api config charts || { \
		echo "Generated files are out of date; run make manifests generate and commit the result."; exit 1; }
	@test -z "$$(git status --porcelain -- api config charts)" || { \
		git status --porcelain -- api config charts; echo "Generated files are untracked; commit them."; exit 1; }
	go mod tidy -diff

# CI tests and scans with go.mod's toolchain, so the image must be built
# with the same Go, and the devcontainer develops with it.
.PHONY: verify-go-version
verify-go-version: ## Fail unless the Dockerfile and the devcontainer use go.mod's toolchain
	@want=$$(awk '$$1 == "toolchain" {sub(/^go/, "", $$2); print $$2}' go.mod); \
	[ -n "$$want" ] || { echo "go.mod names no toolchain"; exit 1; }; \
	for f in Dockerfile .devcontainer/devcontainer.json; do \
		grep -qE "golang:$${want//./\\.}@sha256:" "$$f" || { \
			echo "$$f doesn't use golang:$$want, go.mod's toolchain; bump them together"; exit 1; }; \
	done

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

GOVULNCHECK_VERSION ?= v1.9.0

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

# Versioning recipes read VERSION from the environment rather than having
# make expand it into them, and check its form first, so a tag can't run as
# shell. A pre-release, such as 0.2.0-rc.1, rehearses its version's release:
# its notes are that version's changelog section, which needn't be dated yet.
CHECK_VERSION = [[ "$$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$$ ]] || { \
	echo "VERSION must be a version without a v, such as 0.2.0 or 0.2.0-rc.1, not '$$VERSION'" >&2; exit 1; }

.PHONY: bump
bump: ## Bump the version in the chart, the kustomize image, and docs, and date a release's changelog. Usage: make bump VERSION=0.1.2
	@$(CHECK_VERSION)
	@OLD="$$(awk '/^version:/ {print $$2}' charts/hybernate/Chart.yaml)" \
		perl -pi -e 's/(?<![\d.])\Q$$ENV{OLD}\E(?!\.?[\w-])/$$ENV{VERSION}/g' $(VERSIONED_DOCS)
	@perl -pi -e 's/^version:.*/version: $$ENV{VERSION}/' charts/hybernate/Chart.yaml
	@perl -pi -e 's/^appVersion:.*/appVersion: "v$$ENV{VERSION}"/' charts/hybernate/Chart.yaml
	@perl -pi -e 's/^  newTag:.*/  newTag: v$$ENV{VERSION}/' config/manager/kustomization.yaml
	@[[ "$$VERSION" == *-* ]] || TODAY="$$(date -u +%F)" \
		perl -pi -e 's/^## \[\Q$$ENV{VERSION}\E\] - Unreleased$$/## [$$ENV{VERSION}] - $$ENV{TODAY}/' CHANGELOG.md
	@echo "Bumped to $$VERSION"

.PHONY: verify-version
verify-version: ## Fail unless the chart, the kustomize image, the docs and the changelog name VERSION. Usage: make verify-version VERSION=0.1.2
	@$(CHECK_VERSION)
	@grep -qxF "version: $$VERSION" charts/hybernate/Chart.yaml || { echo "Chart.yaml's version isn't $$VERSION"; exit 1; }
	@grep -qxF "appVersion: \"v$$VERSION\"" charts/hybernate/Chart.yaml || { echo "Chart.yaml's appVersion isn't v$$VERSION"; exit 1; }
	@grep -qxF "  newTag: v$$VERSION" config/manager/kustomization.yaml || { echo "config/manager's image isn't v$$VERSION"; exit 1; }
	@# The install and verify commands name the version whole: after
	@# --version, or as the image's or the chart's tag.
	@pattern="(--version |hybernate:v?)$${VERSION//./\\.}([^0-9A-Za-z.-]|$$)"; \
	for f in $(VERSIONED_DOCS); do \
		grep -qE -- "$$pattern" "$$f" || { echo "$$f doesn't name $$VERSION; run make bump VERSION=$$VERSION"; exit 1; }; \
	done
	@base="$${VERSION%%-*}"; heading="^## \[$${base//./\\.}\] - "; \
	if [[ "$$VERSION" == *-* ]]; then \
		grep -qE -- "$$heading" CHANGELOG.md || { echo "CHANGELOG.md has no section for $$base"; exit 1; }; \
	else \
		grep -qE -- "$$heading[0-9]{4}-[0-9]{2}-[0-9]{2}$$" CHANGELOG.md || { \
			echo "CHANGELOG.md's section for $$base isn't dated; run make bump VERSION=$$VERSION"; exit 1; }; \
	fi

.PHONY: release
release: bump ## Bump version, commit, and tag. Usage: make release VERSION=0.1.2
	@$(MAKE) --no-print-directory verify-version
	@git add charts/hybernate/Chart.yaml config/manager/kustomization.yaml CHANGELOG.md $(VERSIONED_DOCS)
	@# Versions bumped in an earlier commit leave nothing to commit, and the
	@# release is tagged as it stands.
	@git diff --cached --quiet || git commit -m "chore(release): bump version to v$$VERSION"
	@git tag -a "v$$VERSION" -m "v$$VERSION"
	@if [[ "$$VERSION" == *-* ]]; then \
		echo "Tagged v$$VERSION. Run 'git push origin v$$VERSION' to rehearse the release."; \
	else \
		echo "Tagged v$$VERSION. Run 'git push origin main v$$VERSION' to release."; \
	fi

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
	$(CONTAINER_TOOL) build --build-arg VERSION=$(if $(VERSION),v$(VERSION),dev) -t ${IMG} .

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
	@# A builder left by an interrupted run would make create fail.
	@$(CONTAINER_TOOL) buildx rm hybernate-builder >/dev/null 2>&1 || true
	$(CONTAINER_TOOL) buildx create --name hybernate-builder --use
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --build-arg VERSION=$(if $(VERSION),v$(VERSION),dev) --tag ${IMG} . || { \
		$(CONTAINER_TOOL) buildx rm hybernate-builder; exit 1; }
	$(CONTAINER_TOOL) buildx rm hybernate-builder

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
CONTROLLER_TOOLS_VERSION ?= v0.22.0

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
