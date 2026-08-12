VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell if [ -n "$$SOURCE_DATE_EPOCH" ]; then date -u -d "@$$SOURCE_DATE_EPOCH" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || date -u -r "$$SOURCE_DATE_EPOCH" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null; else date -u +"%Y-%m-%dT%H:%M:%SZ"; fi)
GO ?= go
GOFMT_DEFAULT := $(shell command -v "$$($(GO) env GOROOT 2>/dev/null)/bin/gofmt" 2>/dev/null || command -v gofmt 2>/dev/null)
GOFMT ?= $(GOFMT_DEFAULT)
PROJECT_GOFLAGS := -trimpath -buildvcs=false
PROJECT_LDFLAGS := -buildid= -B none \
	-X 'github.com/alibaba/opensandbox/internal/version.Version=$(VERSION)' \
	-X 'github.com/alibaba/opensandbox/internal/version.BuildTime=$(BUILD_TIME)' \
	-X 'github.com/alibaba/opensandbox/internal/version.GitCommit=$(GIT_COMMIT)'
GO_BUILD_FLAGS := $(strip $(GOFLAGS) $(PROJECT_GOFLAGS))
GO_LDFLAGS := $(strip $(LDFLAGS) $(PROJECT_LDFLAGS))

.PHONY: fmt
fmt:
	@command -v "$(GOFMT)" >/dev/null 2>&1 || { echo "gofmt not found: $(GOFMT)" >&2; exit 1; }
	"$(GOFMT)" -w . ../internal

.PHONY: fmt-check
fmt-check:
	@set -e; \
	command -v "$(GOFMT)" >/dev/null 2>&1 || { echo "gofmt not found: $(GOFMT)" >&2; exit 1; }; \
	files="$$("$(GOFMT)" -l . ../internal)"; \
	test -z "$$files" || { echo "gofmt is required for:" >&2; echo "$$files" >&2; exit 1; }

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: test
test:
	$(GO) test -race -cover ./...

.PHONY: integration-compile
integration-compile:
	$(GO) test -tags=integration -run '^$$' ./...

.PHONY: build
build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/nodeagent .
	CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/nodeagent-oss-cleanup ./cmd/oss-cleanup

.PHONY: check
check: fmt-check vet test integration-compile build

.PHONY: clean
clean:
	rm -rf bin
