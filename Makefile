# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# YEAR replaces the placeholder in the boilerplate header. It's fixed because a date
# here would make generated files look stale every January.
YEAR ?= 2026

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
manifests: controller-gen ## Generate ClusterRole and CustomResourceDefinition objects, and copy them into the chart.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd paths="./..." output:crd:artifacts:config=config/crd/bases
	cp config/crd/bases/*.yaml charts/kardigan/files/crds/
	cp config/rbac/role.yaml charts/kardigan/files/manager-role.yaml

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# The e2e tests assume Kind is pre-installed, and build and load the manager image
# locally. kubectl kuberc is disabled by default for test isolation; enable with:
# - KUBECTL_KUBERC=true
# They use their own kubeconfig, so they can't touch whichever cluster your current
# kubectl context points at, which matters because they delete the CRDs at the end.
KIND_CLUSTER ?= kardigan-test-e2e
E2E_KUBECONFIG ?= $(LOCALBIN)/e2e.kubeconfig

.PHONY: setup-test-e2e
setup-test-e2e: $(LOCALBIN) ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) --kubeconfig "$(E2E_KUBECONFIG)" ;; \
	esac
	@$(KIND) get kubeconfig --name $(KIND_CLUSTER) > "$(E2E_KUBECONFIG)"

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	@trap '$(MAKE) cleanup-test-e2e; git checkout -- config/manager/kustomization.yaml' EXIT; \
	KUBECONFIG="$(E2E_KUBECONFIG)" KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

.PHONY: vulncheck
vulncheck: ## Scan the code and its dependencies for known vulnerabilities.
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: lint-actions
lint-actions: ## Check the GitHub Actions workflows.
	go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

.PHONY: image-scan
image-scan: docker-build ## Build the image and fail on any fixable high or critical vulnerability.
	$(CONTAINER_TOOL) run --rm -v /var/run/docker.sock:/var/run/docker.sock $(GRYPE_IMAGE) \
		docker:${IMG} --only-fixed --fail-on high

.PHONY: verify-tidy
verify-tidy: ## Fail if go.mod or go.sum need tidying.
	go mod tidy -diff

.PHONY: verify-generated
verify-generated: manifests generate ## Fail if generated files are stale.
	@paths="api config/crd config/rbac charts/kardigan/files"; \
	if ! git diff --exit-code -- $$paths || [ -n "$$(git ls-files --others --exclude-standard -- $$paths)" ]; then \
		echo "Error: generated files are stale. Run 'make manifests generate' and commit the result."; exit 1; \
	fi

.PHONY: verify
verify: verify-tidy verify-generated lint-config lint lint-actions test chart-lint vulncheck ## Run the fast checks, as CI does.

.PHONY: ci
ci: verify image-scan test-e2e ## Run every check CI runs, plus an image scan. Needs Docker and Kind.

##@ Local stack

# The tests below wipe the stack first. They all add or change series, and Mimir counts
# a series as active for 20 minutes after its last sample, so leftovers would skew them.
STACK_DIR = hack/local-stack
GRAFANA_LOCAL = http://localhost:9000
MIMIR_LOCAL = http://localhost:9009
MIMIRTOOL_OUT = $(LOCALBIN)/mimirtool-out

.PHONY: stack-up
stack-up: ## Start Mimir, Prometheus, Grafana and the synthetic teams, and wait for their data.
	cd $(STACK_DIR) && $(CONTAINER_TOOL) compose up -d
	MIMIR_URL=$(MIMIR_LOCAL) GRAFANA_URL=$(GRAFANA_LOCAL) hack/wait-for-stack.sh

.PHONY: stack-down
stack-down: ## Stop the local stack and wipe its data.
	cd $(STACK_DIR) && $(CONTAINER_TOOL) compose down -v

.PHONY: mimirtool-analyze
mimirtool-analyze: stack-up $(LOCALBIN) ## Run mimirtool against the local stack, for test-integration to compare with.
	mkdir -p "$(MIMIRTOOL_OUT)"
	$(MIMIRTOOL) analyze grafana --address=http://kardigan-grafana:3000 --key=unused
	$(MIMIRTOOL) analyze ruler --address=http://localhost:8080 --id=demo
	$(MIMIRTOOL) analyze prometheus --address=http://localhost:8080/prometheus --id=demo --concurrency=2 \
		--grafana-metrics-file=metrics-in-grafana.json --ruler-metrics-file=metrics-in-ruler.json

.PHONY: test-integration
test-integration: ## Wipe the local stack, test the clients against it, and compare results with mimirtool.
	$(MAKE) stack-down mimirtool-analyze
	KARDIGAN_MIMIR_URL=$(MIMIR_LOCAL) KARDIGAN_GRAFANA_URL=$(GRAFANA_LOCAL) \
	KARDIGAN_MIMIRTOOL_METRICS="$(MIMIRTOOL_OUT)/prometheus-metrics.json" \
	go test -tags=integration ./internal/mimir/ ./internal/recommend/ -run 'Against' -count=1 -v

.PHONY: test-stack
test-stack: manifests kustomize ## Wipe the local stack, and test the deployed operator against it in a throwaway Kind cluster. Stops Mimir and Grafana briefly.
	$(MAKE) stack-down stack-up
	KIND=$(KIND) hack/test-stack.sh stack

.PHONY: test-extended
test-extended: manifests helm ## Wipe the local stack, and test the Helm chart, its NetworkPolicy, and a snippet through the Prometheus Operator.
	$(MAKE) stack-down stack-up
	KIND=$(KIND) HELM="$(HELM)" hack/test-stack.sh extended

.PHONY: test-release
test-release: manifests helm kustomize ## Build the release artefacts, push them to a local registry, and check them.
	HELM="$(HELM)" REGISTRY_IMAGE=$(REGISTRY_IMAGE) hack/test-release.sh

.PHONY: test-all
test-all: test-integration test-stack test-extended test-release ## Run every test against the stack. Run before a release.

.PHONY: secrets-scan
secrets-scan: ## Scan the history of every branch for secrets.
	$(CONTAINER_TOOL) run --rm -u "$$(id -u):$$(id -g)" -v "$(CURDIR):/repo:ro" $(GITLEAKS_IMAGE) git /repo --log-opts=--all --redact --no-banner

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -trimpath -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# Override BASE_IMAGE to build from another registry, e.g.
# make docker-build IMG=<img> BASE_IMAGE=docker.io/library/golang:1.26
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# The platforms a release publishes. Pushing needs a builder that can export
# multiple platforms (docker buildx create --use) and a registry login.
PLATFORMS ?= linux/amd64,linux/arm64
.PHONY: docker-buildx
docker-buildx: ## Build and push the multi-platform image, with an SBOM and provenance.
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --sbom=true --provenance=mode=max \
		$(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --tag ${IMG} .

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Chart

CHART_LINT_ARGS = --set mimir.url=http://mimir:8080

.PHONY: chart-lint
chart-lint: helm ## Lint the Helm chart and check that it refuses to render without a Mimir URL.
	"$(HELM)" lint charts/kardigan $(CHART_LINT_ARGS)
	"$(HELM)" template kardigan charts/kardigan $(CHART_LINT_ARGS) > /dev/null
	@! "$(HELM)" template kardigan charts/kardigan > /dev/null 2>&1 || { \
		echo "Error: the chart rendered without mimir.url"; exit 1; }

##@ Deployment

# The Kustomize overlay that deploy and undeploy use. config/kind points the
# operator at the local stack.
KUSTOMIZE_OVERLAY ?= config/default

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
	"$(KUSTOMIZE)" build $(KUSTOMIZE_OVERLAY) | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build $(KUSTOMIZE_OVERLAY) | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

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
HELM ?= $(LOCALBIN)/helm

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.22.0
HELM_VERSION ?= v4.3.1
GOVULNCHECK_VERSION ?= v1.9.0
ACTIONLINT_VERSION ?= v1.7.12
GRYPE_IMAGE ?= anchore/grype:v0.121.1@sha256:6b06708b036f74b2cd70e94b706b4586af96d1932e6ac46255940b2e39dd44a9
GITLEAKS_IMAGE ?= ghcr.io/gitleaks/gitleaks:v8.30.1@sha256:c00b6bd0aeb3071cbcb79009cb16a60dd9e0a7c60e2be9ab65d25e6bc8abbb7f
REGISTRY_IMAGE ?= registry:3@sha256:d4032d4cc323c1731c707931ecada07a53076eac38e3b61ae57533f170f2c6c3
MIMIRTOOL_IMAGE ?= grafana/mimirtool:3.2.4@sha256:abc0210ebadecaf0b07ef0801b80f670ac7b050a797b987f9ad45362ca85fb4a
# Shares Mimir's network, so localhost:8080 is Mimir and Grafana resolves by name.
MIMIRTOOL = $(CONTAINER_TOOL) run --rm --network container:kardigan-mimir -u "$$(id -u):$$(id -g)" \
	-v "$(MIMIRTOOL_OUT):/out" -w /out $(MIMIRTOOL_IMAGE)

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.15.0
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: helm
helm: $(HELM) ## Download helm locally if necessary.
$(HELM): $(LOCALBIN)
	$(call go-install-tool,$(HELM),helm.sh/helm/v4/cmd/helm,$(HELM_VERSION))

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
	@if test -f .custom-gcl.yml; then \
		echo "Building custom golangci-lint with plugins..."; \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom; \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	fi

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
