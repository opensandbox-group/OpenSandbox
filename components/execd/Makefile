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

.PHONY: install-golint
install-golint:
	@if ! command -v golangci-lint &> /dev/null; then \
  		echo "installing golangci-lint..."; \
  		go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest; \
  	else \
  	    echo "golangci-lint already installed"; \
	fi

.PHONY: golint
golint: fmt install-golint
	"$$(go env GOPATH)/bin/golangci-lint" run -v ./...

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || git rev-parse --short HEAD 2>/dev/null || echo "dev")
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
BUILD_TIME ?= $(shell if [ -n "$$SOURCE_DATE_EPOCH" ]; then date -u -d "@$$SOURCE_DATE_EPOCH" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || date -u -r "$$SOURCE_DATE_EPOCH" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null; else date -u +"%Y-%m-%dT%H:%M:%SZ"; fi)
SESSION_GATE_BINARY := bin/opensandbox-session-gate
SESSION_GATE_SOURCE := native/session-gate.c
SESSION_GATE_SOURCE_INSTALL_DIR := /usr/local/libexec
SESSION_GATE_RUNTIME_DIR := /opt/opensandbox
SESSION_GATE_CFLAGS ?= $(CFLAGS) -O2 -Wall -Wextra -Werror
SESSION_GATE_LDFLAGS ?= -static -s
LAUNCHER_BINARY := bin/opensandbox-launcher
LAUNCHER_SOURCE := native/launcher.c
LAUNCHER_RUNTIME_DIR := /opt/opensandbox
INSTALL ?= install
DESTDIR ?=
ifeq ($(strip $(DESTDIR)),)
SESSION_GATE_INSTALL_OWNER_ARGS := -o root -g root
else
SESSION_GATE_INSTALL_OWNER_ARGS :=
endif
PROJECT_GOFLAGS := -trimpath -buildvcs=false
PROJECT_LDFLAGS := -buildid= -B none -X 'github.com/alibaba/opensandbox/internal/version.Version=$(VERSION)' \
	-X 'github.com/alibaba/opensandbox/internal/version.BuildTime=$(BUILD_TIME)' \
	-X 'github.com/alibaba/opensandbox/internal/version.GitCommit=$(GIT_COMMIT)'
GO_BUILD_FLAGS := $(strip $(GOFLAGS) $(PROJECT_GOFLAGS))
GO_LDFLAGS := $(strip $(LDFLAGS) $(PROJECT_LDFLAGS))

.PHONY: build-session-gate install-session-gate build-launcher install-launcher
build-session-gate:
	@set -eu; \
	host_goos="$$(go env GOHOSTOS)"; \
	host_goarch="$$(go env GOHOSTARCH)"; \
	target_goos="$(if $(GOOS),$(GOOS),$$(go env GOOS))"; \
	target_goarch="$(if $(GOARCH),$(GOARCH),$$(go env GOARCH))"; \
	if [ "$$target_goos" != "linux" ]; then \
		echo "Skipping session gate: isolated sessions require Linux (target=$$target_goos/$$target_goarch)"; \
		exit 0; \
	fi; \
	if [ "$$host_goos/$$host_goarch" != "$$target_goos/$$target_goarch" ]; then \
		echo "session gate cross-build is unsupported (host=$$host_goos/$$host_goarch, target=$$target_goos/$$target_goarch)" >&2; \
		echo "use the execd Docker build for multi-architecture Linux artifacts" >&2; \
		exit 1; \
	fi; \
	mkdir -p bin; \
	$(CC) $(CPPFLAGS) $(SESSION_GATE_CFLAGS) "$(SESSION_GATE_SOURCE)" \
		$(SESSION_GATE_LDFLAGS) -o "$(SESSION_GATE_BINARY).tmp"; \
	mv -f "$(SESSION_GATE_BINARY).tmp" "$(SESSION_GATE_BINARY)"

install-session-gate:
	@if [ "$$(uname -s)" != "Linux" ]; then \
		echo "install-session-gate requires Linux" >&2; \
		exit 1; \
	fi
	@if [ ! -x "$(SESSION_GATE_BINARY)" ]; then \
		echo "$(SESSION_GATE_BINARY) is missing; run make build-session-gate first" >&2; \
		exit 1; \
	fi
	@if [ -z "$(DESTDIR)" ] && [ "$$(id -u)" -ne 0 ]; then \
		echo "install-session-gate requires root unless DESTDIR is set" >&2; \
		exit 1; \
	fi
	@umask 022; mkdir -p \
		"$(DESTDIR)$(SESSION_GATE_SOURCE_INSTALL_DIR)" \
		"$(DESTDIR)$(SESSION_GATE_RUNTIME_DIR)"
	@if [ -z "$(DESTDIR)" ]; then \
		chown root:root \
			"$(SESSION_GATE_SOURCE_INSTALL_DIR)" \
			"$(SESSION_GATE_RUNTIME_DIR)"; \
	fi
	chmod go-w \
		"$(DESTDIR)$(SESSION_GATE_SOURCE_INSTALL_DIR)" \
		"$(DESTDIR)$(SESSION_GATE_RUNTIME_DIR)"
	$(INSTALL) $(SESSION_GATE_INSTALL_OWNER_ARGS) -m 0555 "$(SESSION_GATE_BINARY)" \
		"$(DESTDIR)$(SESSION_GATE_SOURCE_INSTALL_DIR)/opensandbox-session-gate"
	$(INSTALL) $(SESSION_GATE_INSTALL_OWNER_ARGS) -m 0555 "$(SESSION_GATE_BINARY)" \
		"$(DESTDIR)$(SESSION_GATE_RUNTIME_DIR)/opensandbox-session-gate"

build-launcher:
	@set -eu; \
	host_goos="$$(go env GOHOSTOS)"; \
	host_goarch="$$(go env GOHOSTARCH)"; \
	target_goos="$(if $(GOOS),$(GOOS),$$(go env GOOS))"; \
	target_goarch="$(if $(GOARCH),$(GOARCH),$$(go env GOARCH))"; \
	if [ "$$target_goos" != "linux" ]; then \
		echo "Skipping launcher: hardening requires Linux (target=$$target_goos/$$target_goarch)"; \
		exit 0; \
	fi; \
	if [ "$$host_goos/$$host_goarch" != "$$target_goos/$$target_goarch" ]; then \
		echo "launcher cross-build is unsupported (host=$$host_goos/$$host_goarch, target=$$target_goos/$$target_goarch)" >&2; \
		echo "use the execd Docker build for multi-architecture Linux artifacts" >&2; \
		exit 1; \
	fi; \
	mkdir -p bin; \
	$(CC) $(CPPFLAGS) $(SESSION_GATE_CFLAGS) "$(LAUNCHER_SOURCE)" \
		$(SESSION_GATE_LDFLAGS) -o "$(LAUNCHER_BINARY).tmp"; \
	mv -f "$(LAUNCHER_BINARY).tmp" "$(LAUNCHER_BINARY)"

install-launcher:
	@if [ "$$(uname -s)" != "Linux" ]; then \
		echo "install-launcher requires Linux" >&2; \
		exit 1; \
	fi
	@if [ ! -x "$(LAUNCHER_BINARY)" ]; then \
		echo "$(LAUNCHER_BINARY) is missing; run make build-launcher first" >&2; \
		exit 1; \
	fi
	@if [ -z "$(DESTDIR)" ] && [ "$$(id -u)" -ne 0 ]; then \
		echo "install-launcher requires root unless DESTDIR is set" >&2; \
		exit 1; \
	fi
	@umask 022; mkdir -p "$(DESTDIR)$(LAUNCHER_RUNTIME_DIR)"
	@if [ -z "$(DESTDIR)" ]; then \
		chown root:root "$(DESTDIR)$(LAUNCHER_RUNTIME_DIR)"; \
	fi
	chmod go-w "$(DESTDIR)$(LAUNCHER_RUNTIME_DIR)"
	$(INSTALL) $(SESSION_GATE_INSTALL_OWNER_ARGS) -m 0555 "$(LAUNCHER_BINARY)" \
		"$(DESTDIR)$(LAUNCHER_RUNTIME_DIR)/opensandbox-launcher"

.PHONY: build
build: vet build-session-gate build-launcher ## Build execd and the Linux native helpers.
	@mkdir -p bin
	go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/execd main.go

.PHONY: build-ebpf
build-ebpf: ## Build the execd-ebpf observation variant (CGO + cilium/ebpf).
	@if [ "$$(uname -s 2>/dev/null || echo non-linux)" != "Linux" ]; then \
		echo "execd-ebpf requires Linux (BPF attachable host)" >&2; \
		exit 1; \
	fi
	@mkdir -p bin
	CGO_ENABLED=1 go build -tags ebpf $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/execd-ebpf main.go
	@echo "built bin/execd-ebpf"

.PHONY: test-integration
test-integration: ## Run integration tests (Linux + bwrap required).
	go test -v -tags="linux,bwrap" -run Integration ./pkg/runtime/bwrap_test/

.PHONY: multi-build
multi-build: vet ## Cross-compile execd only; use Docker for complete Linux runtimes.
	@mkdir -p bin
	@for os in linux windows darwin; do \
		for arch in amd64 arm64; do \
			out=bin/execd_$(VERSION)_$${os}_$${arch}; \
			[ "$${os}" = "windows" ] && out="$${out}.exe"; \
			echo ">> building $${os}/$${arch} -> $${out}"; \
			GOOS=$${os} GOARCH=$${arch} CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o "$${out}" main.go || exit $$?; \
		done; \
	done
