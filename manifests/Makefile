# Helm chart tooling for OpenSandbox. All charts live under manifests/charts.

# VERSION feeds the image tag overrides for the helm-template / helm-install /
# helm-upgrade / helm-dry-run targets below (rendered as v$(VERSION)). Leave
# it empty to use each chart's default image tag (release-<appVersion>).
VERSION ?=

# IMAGE_TAG_BASE defines the registry namespace and part of the image name for
# remote images. Used by the helm-template/install targets below.
IMAGE_TAG_BASE ?= sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/controller

# Helm chart configuration
HELM_CHART_PATH ?= charts/controller
# Chart/app versions are derived from the chart's Chart.yaml so packaging never
# drifts from the committed versions. Override HELM_CHART_VERSION to repackage
# under a different version.
HELM_CHART_VERSION ?= $(shell sed -n 's/^version:[[:space:]]*//p' $(HELM_CHART_PATH)/Chart.yaml | head -n1)
HELM_CHART_APP_VERSION ?= $(shell sed -n 's/^appVersion:[[:space:]]*"\(.*\)".*/\1/p' $(HELM_CHART_PATH)/Chart.yaml | head -n1)

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

.PHONY: helm-gen-fast-sandbox-crds
helm-gen-fast-sandbox-crds: ## Sync fast-sandbox CRDs from the pinned checkout into charts/base/files/fast-sandbox-crds.yaml.
	@release/build-fast-sandbox.sh --no-build --sync-crds

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint all Helm charts and verify dependencies
	@echo "Linting Helm charts..."
	helm lint charts/base
	helm lint charts/ingress-gateway
	helm lint charts/server
	helm lint charts/controller
	helm lint charts/node-agent
	helm lint charts/fast-sandbox
	helm dependency build charts/opensandbox
	helm lint charts/opensandbox

.PHONY: helm-template
helm-template: ## Generate Kubernetes manifests from the controller chart
	@echo "Generating manifests from Helm chart..."
	helm template opensandbox-controller $(HELM_CHART_PATH) \
		--set controller.image.repository=$(IMAGE_TAG_BASE) \
		$(if $(VERSION),--set controller.image.tag=v$(VERSION),)

.PHONY: helm-template-debug
helm-template-debug: ## Generate Kubernetes manifests with debug output
	@echo "Generating manifests from Helm chart with debug..."
	helm template opensandbox-controller $(HELM_CHART_PATH) \
		--set controller.image.repository=$(IMAGE_TAG_BASE) \
		$(if $(VERSION),--set controller.image.tag=v$(VERSION),) \
		--debug

.PHONY: helm-package
helm-package: ## Package the Helm chart (version derived from Chart.yaml)
	@echo "Packaging Helm chart..."
	@mkdir -p dist
	helm package $(HELM_CHART_PATH) -d dist/ --version $(HELM_CHART_VERSION) $(if $(HELM_CHART_APP_VERSION),--app-version $(HELM_CHART_APP_VERSION),)

.PHONY: helm-install
helm-install: ## Install the Helm charts (base CRDs first, then the controller)
	@echo "Installing Helm charts..."
	helm upgrade --install base charts/base
	helm install opensandbox-controller $(HELM_CHART_PATH) \
		--set controller.image.repository=$(IMAGE_TAG_BASE) \
		$(if $(VERSION),--set controller.image.tag=v$(VERSION),) \
		--namespace opensandbox-system \
		--create-namespace

.PHONY: helm-upgrade
helm-upgrade: ## Upgrade the controller release
	@echo "Upgrading Helm chart..."
	helm upgrade opensandbox-controller $(HELM_CHART_PATH) \
		--set controller.image.repository=$(IMAGE_TAG_BASE) \
		$(if $(VERSION),--set controller.image.tag=v$(VERSION),) \
		--namespace opensandbox-system

.PHONY: helm-uninstall
helm-uninstall: ## Uninstall the controller release
	@echo "Uninstalling Helm chart..."
	helm uninstall opensandbox-controller --namespace opensandbox-system

.PHONY: helm-dry-run
helm-dry-run: ## Perform a dry-run install of the controller chart
	@echo "Performing dry-run installation..."
	helm install opensandbox-controller $(HELM_CHART_PATH) \
		--set controller.image.repository=$(IMAGE_TAG_BASE) \
		$(if $(VERSION),--set controller.image.tag=v$(VERSION),) \
		--namespace opensandbox-system \
		--create-namespace \
		--dry-run --debug

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

.PHONY: helm-all
helm-all: helm-lint helm-package ## Run all Helm-related tasks (lint and package)
