.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go mod tidy && go mod vendor
	go vet ./...

.PHONY: test
test: vet ## Run tests
	go test -v -coverpkg=./... ./pkg/...

##@ Linter

# Go installs tools in GOBIN, or in the first GOPATH entry's bin directory.
GO_BIN = $(or $(shell go env GOBIN),$(shell go env GOPATH | cut -d: -f1)/bin)
GOLANGCI_LINT ?= $(or $(shell command -v golangci-lint 2>/dev/null),$(GO_BIN)/golangci-lint)

.PHONY: install-golint
install-golint:
	@if ! command -v "$(GOLANGCI_LINT)" >/dev/null 2>&1; then \
		echo "installing golangci-lint..."; \
		go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest; \
	else \
		echo "golangci-lint already installed"; \
	fi

.PHONY: golint
golint: fmt install-golint
	"$(GOLANGCI_LINT)" run -v --fix ./...

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
BUILD_TIME ?= $(shell if [ -n "$$SOURCE_DATE_EPOCH" ]; then date -u -d "@$$SOURCE_DATE_EPOCH" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || date -u -r "$$SOURCE_DATE_EPOCH" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null; else date -u +"%Y-%m-%dT%H:%M:%SZ"; fi)
PROJECT_GOFLAGS := -trimpath -buildvcs=false
PROJECT_LDFLAGS := -buildid= -B none -X 'github.com/alibaba/opensandbox/internal/version.Version=$(VERSION)' \
	-X 'github.com/alibaba/opensandbox/internal/version.BuildTime=$(BUILD_TIME)' \
	-X 'github.com/alibaba/opensandbox/internal/version.GitCommit=$(GIT_COMMIT)'
GO_BUILD_FLAGS := $(strip $(GOFLAGS) $(PROJECT_GOFLAGS))
GO_LDFLAGS := $(strip $(LDFLAGS) $(PROJECT_LDFLAGS))

.PHONY: build
build: vet ## Build the binary.
	@mkdir -p bin
	go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/router main.go

.PHONY: clean
clean: ## Clean build artifacts.
	rm -rf bin/ vendor/
