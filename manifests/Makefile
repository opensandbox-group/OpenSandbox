# Helm chart tooling for OpenSandbox. All charts live under manifests/charts.

# VERSION feeds the Helm chart version and image tags. To build for a specific
# version, pass it as an arg (e.g make helm-package VERSION=0.0.2) or export it
# (e.g export VERSION=0.0.2).
VERSION ?= 0.1.0

# IMAGE_TAG_BASE defines the registry namespace and part of the image name for
# remote images. Used by the helm-template/install targets below.
IMAGE_TAG_BASE ?= sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/controller

# Helm chart configuration
HELM_CHART_PATH ?= charts/controller
HELM_CHART_VERSION ?= $(VERSION)

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Charts

.PHONY: helm-gen-crds
helm-gen-crds: ## Sync generated CRDs from kubernetes/config/crd/bases into charts/base/files/crds.yaml.
	@mkdir -p charts/base/files
	@rm -f charts/base/files/crds.yaml
	@for f in $(sort $(wildcard ../kubernetes/config/crd/bases/*.yaml)); do \
		printf '%s\n---\n' "$$(awk 'NR==1 && $$0=="---" {next} {print}' $$f)" >> charts/base/files/crds.yaml; \
	done

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint all Helm charts and verify dependencies
	@echo "Linting Helm charts..."
	helm lint charts/base
	helm lint charts/ingress-gateway
	helm lint charts/server
	helm lint charts/controller
	helm lint charts/node-agent
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
helm-install: ## Install the Helm charts (base CRDs first, then the controller)
	@echo "Installing Helm charts..."
	helm upgrade --install base charts/base
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
