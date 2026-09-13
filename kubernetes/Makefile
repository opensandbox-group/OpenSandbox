# VERSION defines the project version. It feeds the Helm chart version and
# image tags. To build for a specific version, pass it as an arg
# (e.g make helm-package VERSION=0.0.2) or export it (e.g export VERSION=0.0.2).
VERSION ?= 0.1.0

# IMAGE_TAG_BASE defines the registry namespace and part of the image name for remote images.
# This variable is used to construct full image tags for build and push targets.
IMAGE_TAG_BASE ?= sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/controller

# Image URL to use all building/pushing image targets
# CONTROLLER_IMG defines the image for the controller manager.
CONTROLLER_IMG ?= controller:dev
# TASK_EXECUTOR_IMG defines the image for the task-executor service.
TASK_EXECUTOR_IMG ?= task-executor:dev
# IMAGE_COMMITTER_IMG defines the image for the default image-committer service.
IMAGE_COMMITTER_IMG ?= image-committer:dev
# SNAPSHOT_REGISTRY defines the OCI registry used by the controller for snapshot images.
SNAPSHOT_REGISTRY ?= docker-registry.default.svc.cluster.local:5000

# Go installs tools in GOBIN, or in the first GOPATH entry's bin directory.
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH | cut -d: -f1)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# DOCKER_BUILD_ARGS defines additional arguments to pass to docker build.
# For example, in some environments you may need: DOCKER_BUILD_ARGS=--network=host
DOCKER_BUILD_ARGS ?=

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
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
KIND_CLUSTER ?= sandbox-k8s-test-e2e
KIND_K8S_VERSION ?= v1.22.4
GINKGO_ARGS ?=
E2E_TIMEOUT ?= 60m

.PHONY: install-kind
install-kind: ## Install Kind using go install if not already installed
	@if ! command -v "$(KIND)" >/dev/null 2>&1; then \
		echo "Installing Kind..."; \
		GOBIN="$(GOBIN)" go install sigs.k8s.io/kind@v0.20.0; \
	fi
	@"$(KIND)" version

.PHONY: setup-test-e2e
setup-test-e2e: install-kind ## Set up a Kind cluster for e2e tests if it does not exist
	@case "$$("$(KIND)" get clusters 2>/dev/null || echo '')" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)' with Kubernetes version $(KIND_K8S_VERSION)..."; \
			"$(KIND)" create cluster --name $(KIND_CLUSTER) --image kindest/node:$(KIND_K8S_VERSION) ;; \
	esac

.PHONY: test-e2e-core
test-e2e-core: setup-test-e2e manifests generate fmt vet ## Run only Core-labelled e2e tests
	CONTROLLER_IMG=$(CONTROLLER_IMG) TASK_EXECUTOR_IMG=$(TASK_EXECUTOR_IMG) E2E_LABEL_FILTER=Core \
		KIND_CLUSTER=$(KIND_CLUSTER) go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter=Core -timeout $(E2E_TIMEOUT) $(GINKGO_ARGS)
	$(MAKE) cleanup-test-e2e

.PHONY: test-e2e-pause-resume
test-e2e-pause-resume: setup-test-e2e manifests generate fmt vet ## Run only PauseResume-labelled e2e tests
	CONTROLLER_IMG=$(CONTROLLER_IMG) TASK_EXECUTOR_IMG=$(TASK_EXECUTOR_IMG) E2E_LABEL_FILTER=PauseResume \
		KIND_CLUSTER=$(KIND_CLUSTER) go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter=PauseResume -timeout $(E2E_TIMEOUT) $(GINKGO_ARGS)
	$(MAKE) cleanup-test-e2e

.PHONY: test-e2e-main
test-e2e-main: setup-test-e2e manifests generate fmt vet 
	CONTROLLER_IMG=$(CONTROLLER_IMG) TASK_EXECUTOR_IMG=$(TASK_EXECUTOR_IMG) E2E_LABEL_FILTER= \
		KIND_CLUSTER=$(KIND_CLUSTER) go test ./test/e2e/ -v -ginkgo.v -timeout $(E2E_TIMEOUT) $(GINKGO_ARGS)
	$(MAKE) cleanup-test-e2e

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind. Use GINKGO_ARGS to pass additional arguments.
	CONTROLLER_IMG=$(CONTROLLER_IMG) TASK_EXECUTOR_IMG=$(TASK_EXECUTOR_IMG) E2E_LABEL_FILTER= \
		KIND_CLUSTER=$(KIND_CLUSTER) go test ./test/e2e/ -v -ginkgo.v -timeout $(E2E_TIMEOUT) $(GINKGO_ARGS)
	CONTROLLER_IMG=$(CONTROLLER_IMG) TASK_EXECUTOR_IMG=$(TASK_EXECUTOR_IMG) E2E_LABEL_FILTER= \
		KIND_CLUSTER=$(KIND_CLUSTER) go test ./test/e2e_task/ -v -ginkgo.v -timeout $(E2E_TIMEOUT) $(GINKGO_ARGS)
	$(MAKE) cleanup-test-e2e
	$(MAKE) test-gvisor CONTROLLER_IMG=$(CONTROLLER_IMG) TASK_EXECUTOR_IMG=$(TASK_EXECUTOR_IMG)
	$(MAKE) cleanup-gvisor

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@"$(KIND)" delete cluster --name $(KIND_CLUSTER)


.PHONY: test-e2e-qemu
test-e2e-qemu: ## Run QEMU-in-runc VMState pause/resume E2E (requires Linux amd64 and /dev/kvm)
	bash test/e2e_qemu/run.sh

# Common E2E setup targets - install CRDs and controller for any Kind cluster
.PHONY: install-e2e-deps
install-e2e-deps:
	@echo "Installing OpenSandbox CRDs..."
	@$(MAKE) install
	@echo "Installing OpenSandbox controller..."
	@cd config/manager && $(KUSTOMIZE) edit set image controller=$(CONTROLLER_IMG)
	@$(KUSTOMIZE) build config/default | kubectl apply -f -
	@echo "Waiting for controller to be ready..."
	@kubectl wait --for=condition=available --timeout=120s deployment -n opensandbox-system opensandbox-controller-manager || \
		{ kubectl describe deployment -n opensandbox-system opensandbox-controller-manager; exit 1; }

# RuntimeClass E2E testing - gVisor
GVISOR_KIND_CLUSTER ?= gvisor-test
GVISOR_KIND_IMAGE ?= kindest/node:v1.27.3

# gVisor versions
GVISOR_VERSION ?= 20260112
GVISOR_RUNSC_BIN ?= $(shell pwd)/test/kind/gvisor/runsc
GVISOR_SHIM_BIN ?= $(shell pwd)/test/kind/gvisor/containerd-shim-runsc-v1

.PHONY: download-gvisor
download-gvisor: ## Download gVisor runsc and containerd-shim-runsc-v1 binaries
	@echo "Downloading gVisor runsc (release-$(GVISOR_VERSION))..."
	@mkdir -p $(dir $(GVISOR_RUNSC_BIN))
	@wget -q "https://storage.googleapis.com/gvisor/releases/release/$(GVISOR_VERSION)/$$(uname -m)/runsc" -O $(GVISOR_RUNSC_BIN)
	@chmod +x $(GVISOR_RUNSC_BIN)
	@echo "Downloading containerd-shim-runsc-v1..."
	@wget -q "https://storage.googleapis.com/gvisor/releases/release/$(GVISOR_VERSION)/$$(uname -m)/containerd-shim-runsc-v1" -O $(GVISOR_SHIM_BIN)
	@chmod +x $(GVISOR_SHIM_BIN)
	@echo "gVisor binaries downloaded successfully."

.PHONY: setup-gvisor
setup-gvisor: download-gvisor ## Set up Kind cluster with gVisor (runsc) for e2e tests
	@echo "Creating gVisor Kind cluster with runsc binaries from kubernetes/test/kind/gvisor/..."
	@export GVISOR_KIND_CLUSTER=$(GVISOR_KIND_CLUSTER) && \
		export GVISOR_KIND_IMAGE=$(GVISOR_KIND_IMAGE) && \
		export PWD=$$(pwd) && \
		envsubst < test/e2e_runtime/gvisor/testdata/gvisor.yaml.tmpl | \
		"$(KIND)" create cluster --config -
	@echo "Creating runsc.toml on Kind nodes..."
	@for node in $$(docker ps --filter "name=$(GVISOR_KIND_CLUSTER)-" --format "{{.Names}}"); do \
		docker exec $$node sh -c 'mkdir -p /etc/containerd && echo "[runsc]" > /etc/containerd/runsc.toml && echo "  platform = \"ptrace\"" >> /etc/containerd/runsc.toml'; \
	done

.PHONY: cleanup-gvisor
cleanup-gvisor: ## Tear down gVisor Kind cluster
	@"$(KIND)" delete cluster --name $(GVISOR_KIND_CLUSTER) 2>/dev/null || true

# install-gvisor-deps installs CRDs and controller for gVisor tests
.PHONY: install-gvisor-deps
install-gvisor-deps:
	@echo "Building and loading controller image into gVisor Kind cluster..."
	@$(MAKE) docker-build-controller CONTROLLER_IMG=$(CONTROLLER_IMG)
	@"$(KIND)" load docker-image --name $(GVISOR_KIND_CLUSTER) $(CONTROLLER_IMG)
	@$(MAKE) install-e2e-deps CONTROLLER_IMG=$(CONTROLLER_IMG)

.PHONY: test-gvisor
test-gvisor: setup-gvisor install-gvisor-deps ## Run gVisor RuntimeClass e2e tests
	@echo "Installing gVisor RuntimeClass resources..."
	@kubectl apply -f test/e2e_runtime/gvisor/testdata/runtimeclass.yaml
	@echo "Running gVisor E2E tests..."
	CONTROLLER_IMG=$(CONTROLLER_IMG) TASK_EXECUTOR_IMG=$(TASK_EXECUTOR_IMG) \
		KIND_CLUSTER=$(GVISOR_KIND_CLUSTER) go test ./test/e2e_runtime/gvisor -v -ginkgo.v $(GINKGO_ARGS)
	$(MAKE) cleanup-gvisor

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Build

PROJECT_GOFLAGS := -trimpath -buildvcs=false
GO_BUILD_FLAGS := $(strip $(GOFLAGS) $(PROJECT_GOFLAGS))
GO_LDFLAGS := $(strip $(LDFLAGS) -buildid= -B none)

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/manager ./cmd/controller

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/controller

.PHONY: task-executor-build
task-executor-build: ## Build task-executor binary.
	go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/task-executor ./cmd/task-executor

.PHONY: task-executor-run
task-executor-run: ## Run task-executor from your host.
	go run ./cmd/task-executor

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: docker-build-controller

.PHONY: docker-build-controller
docker-build-controller: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build $(DOCKER_BUILD_ARGS) --build-arg PACKAGE=./cmd/controller -t ${CONTROLLER_IMG} .

.PHONY: docker-build-task-executor
docker-build-task-executor: ## Build docker image with task-executor.
	$(CONTAINER_TOOL) build $(DOCKER_BUILD_ARGS) --build-arg PACKAGE=cmd/task-executor/main.go --build-arg USERID=0 -t ${TASK_EXECUTOR_IMG} .

.PHONY: docker-build-image-committer
docker-build-image-committer: ## Build docker image for image commit operations.
	$(CONTAINER_TOOL) build $(DOCKER_BUILD_ARGS) -f Dockerfile.image-committer -t ${IMAGE_COMMITTER_IMG} .

.PHONY: docker-push-image-committer
docker-push-image-committer: ## Push docker image for image-committer.
	$(CONTAINER_TOOL) push ${IMAGE_COMMITTER_IMG}

.PHONY: docker-push
docker-push: docker-push-controller

.PHONY: docker-push-controller
docker-push-controller: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${CONTROLLER_IMG}

.PHONY: docker-push-task-executor
docker-push-task-executor: ## Push docker image with task-executor.
	$(CONTAINER_TOOL) push ${TASK_EXECUTOR_IMG}

.PHONY: docker-run-task-executor
docker-run-task-executor: docker-build-task-executor ## Run task-executor docker image.
	@echo "Running task-executor image: $(TASK_EXECUTOR_IMG) on port 8080"
	@$(CONTAINER_TOOL) run --rm -d -p 8080:8080 --name task-executor-local $(TASK_EXECUTOR_IMG)

.PHONY: docker-stop-task-executor
docker-stop-task-executor: ## Stop task-executor docker container.
	@echo "Stopping task-executor container: task-executor-local"
	-@$(CONTAINER_TOOL) stop task-executor-local || true
	-@$(CONTAINER_TOOL) rm task-executor-local || true

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx CONTROLLER_IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# be able to push the image to your registry (i.e. if you do not set a valid value via CONTROLLER_IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name sandbox-k8s-builder
	$(CONTAINER_TOOL) buildx use sandbox-k8s-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${CONTROLLER_IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm sandbox-k8s-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${CONTROLLER_IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${CONTROLLER_IMG}
	$(KUSTOMIZE) build config/default | \
		sed 's|--snapshot-registry=docker-registry.default.svc.cluster.local:5000|--snapshot-registry=$(SNAPSHOT_REGISTRY)|' | \
		sed 's|--image-committer-image=image-committer:dev|--image-committer-image=$(IMAGE_COMMITTER_IMG)|' | \
		sed 's|--image-committer-pull-secret=|--image-committer-pull-secret=$(IMAGE_COMMITTER_PULL_SECRET)|' | \
		$(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= $(or $(shell command -v kind 2>/dev/null),$(GOBIN)/kind)
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.18.0
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v2.7.2

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
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
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

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef

##@ Helm

# Helm chart configuration
HELM_CHART_PATH ?= charts/opensandbox-controller
HELM_CHART_VERSION ?= $(VERSION)

.PHONY: helm-lint
helm-lint: ## Lint all Helm charts and verify dependencies
	@echo "Linting Helm charts..."
	helm lint charts/opensandbox-controller
	helm lint charts/opensandbox-server
	helm lint charts/opensandbox-node-agent
	helm dependency build charts/opensandbox
	helm lint charts/opensandbox

.PHONY: helm-template
helm-template: ## Generate Kubernetes manifests from Helm chart
	@echo "Generating manifests from Helm chart..."
	helm template opensandbox-controller $(HELM_CHART_PATH) \
		--set controller.image.repository=$(IMAGE_TAG_BASE) \
		--set controller.image.tag=v$(VERSION)

.PHONY: helm-template-debug
helm-template-debug: ## Generate Kubernetes manifests with debug output
	@echo "Generating manifests from Helm chart with debug..."
	helm template opensandbox-controller $(HELM_CHART_PATH) \
		--set controller.image.repository=$(IMAGE_TAG_BASE) \
		--set controller.image.tag=v$(VERSION) \
		--debug

.PHONY: helm-package
helm-package: ## Package the Helm chart
	@echo "Packaging Helm chart..."
	@mkdir -p dist
	helm package $(HELM_CHART_PATH) -d dist/ --version $(HELM_CHART_VERSION) --app-version $(VERSION)

.PHONY: helm-install
helm-install: ## Install the Helm chart
	@echo "Installing Helm chart..."
	helm install opensandbox-controller $(HELM_CHART_PATH) \
		--set controller.image.repository=$(IMAGE_TAG_BASE) \
		--set controller.image.tag=v$(VERSION) \
		--namespace opensandbox-system \
		--create-namespace

.PHONY: helm-upgrade
helm-upgrade: ## Upgrade the Helm chart
	@echo "Upgrading Helm chart..."
	helm upgrade opensandbox-controller $(HELM_CHART_PATH) \
		--set controller.image.repository=$(IMAGE_TAG_BASE) \
		--set controller.image.tag=v$(VERSION) \
		--namespace opensandbox-system

.PHONY: helm-uninstall
helm-uninstall: ## Uninstall the Helm chart
	@echo "Uninstalling Helm chart..."
	helm uninstall opensandbox-controller --namespace opensandbox-system

.PHONY: helm-test
helm-test: ## Run Helm chart tests
	@echo "Running Helm chart tests..."
	helm test opensandbox-controller --namespace opensandbox-system

.PHONY: helm-docs
helm-docs: ## Generate Helm chart documentation for all charts (requires helm-docs)
	@if command -v helm-docs >/dev/null 2>&1; then \
		echo "Generating Helm chart documentation..."; \
		helm-docs --chart-search-root charts/; \
	else \
		echo "helm-docs is not installed. Install it with: go install github.com/norwoodj/helm-docs/cmd/helm-docs@latest"; \
		exit 1; \
	fi

.PHONY: helm-docs-check
helm-docs-check: ## Verify Helm chart documentation is up to date (requires helm-docs)
	@if command -v helm-docs >/dev/null 2>&1; then \
		echo "Checking Helm chart documentation is up to date..."; \
		helm-docs --chart-search-root charts/; \
		if [ -n "$$(git status --porcelain -- charts/ | head -n 1)" ]; then \
			echo "ERROR: Helm chart documentation is out of date. Run 'make helm-docs' and commit the changes."; \
			exit 1; \
		fi; \
		echo "Helm chart documentation is up to date."; \
	else \
		echo "helm-docs is not installed. Install it with: go install github.com/norwoodj/helm-docs/cmd/helm-docs@latest"; \
		exit 1; \
	fi

.PHONY: helm-dry-run
helm-dry-run: ## Perform a dry-run install of the Helm chart
	@echo "Performing dry-run installation..."
	helm install opensandbox-controller $(HELM_CHART_PATH) \
		--set controller.image.repository=$(IMAGE_TAG_BASE) \
		--set controller.image.tag=v$(VERSION) \
		--namespace opensandbox-system \
		--create-namespace \
		--dry-run --debug

.PHONY: helm-all
helm-all: helm-lint helm-package ## Run all Helm-related tasks (lint and package)
