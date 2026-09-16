# execd build automation.
#
# Quick reference:
#
#   make build          # bin/execd + Linux native helpers (helpers skip on non-Linux)
#   make test           # vet + unit tests
#   make golint         # gofmt + golangci-lint
#   make multi-build    # cross-compile check for linux/windows/darwin
#   make build-ebpf     # execd-ebpf observation variant (Linux only)
#
# Native helper cross-builds are unsupported; use the execd Docker build for
# complete multi-architecture Linux runtimes.

# Version stamping
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || git rev-parse --short HEAD 2>/dev/null || echo "dev")
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
BUILD_TIME ?= $(shell if [ -n "$$SOURCE_DATE_EPOCH" ]; then date -u -d "@$$SOURCE_DATE_EPOCH" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || date -u -r "$$SOURCE_DATE_EPOCH" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null; else date -u +"%Y-%m-%dT%H:%M:%SZ"; fi)

# Build flags

# GOFLAGS/LDFLAGS from the environment are honored and merged with the
# reproducible-build defaults.
PROJECT_GOFLAGS := -trimpath -buildvcs=false
PROJECT_LDFLAGS := -buildid= -B none -X 'github.com/alibaba/opensandbox/internal/version.Version=$(VERSION)' \
	-X 'github.com/alibaba/opensandbox/internal/version.BuildTime=$(BUILD_TIME)' \
	-X 'github.com/alibaba/opensandbox/internal/version.GitCommit=$(GIT_COMMIT)'
GO_BUILD_FLAGS := $(strip $(GOFLAGS) $(PROJECT_GOFLAGS))
GO_LDFLAGS := $(strip $(LDFLAGS) $(PROJECT_LDFLAGS))

# Go installs tools in GOBIN, or in the first GOPATH entry's bin directory.
GO_BIN := $(or $(shell go env GOBIN),$(shell go env GOPATH | cut -d: -f1)/bin)
GOLANGCI_LINT ?= $(or $(shell command -v golangci-lint 2>/dev/null),$(GO_BIN)/golangci-lint)

# Linux native helpers

# The session gate and launcher are static C binaries required on Linux only
# (isolated-session gate: fail-closed workload entry; launcher: hardening).
SESSION_GATE_BINARY := bin/opensandbox-session-gate
SESSION_GATE_SOURCE := native/session-gate.c
LAUNCHER_BINARY := bin/opensandbox-launcher
LAUNCHER_SOURCE := native/launcher.c
NATIVE_CFLAGS ?= $(CFLAGS) -O2 -Wall -Wextra -Werror
NATIVE_LDFLAGS ?= -static -s
SESSION_GATE_SOURCE_INSTALL_DIR := /usr/local/libexec
NATIVE_RUNTIME_DIR := /opt/opensandbox
INSTALL ?= install
DESTDIR ?=
# Root ownership is required for the real runtime paths; DESTDIR installs
# (packaging, tests) keep the invoking user's ownership.
ifeq ($(strip $(DESTDIR)),)
NATIVE_INSTALL_OWNER_ARGS := -o root -g root
else
NATIVE_INSTALL_OWNER_ARGS :=
endif

# Canned guard for native helper builds: skip cleanly when the Go target is
# not linux, and refuse host->target cross-builds. Usage: $(call
# native-build-guard,<helper name>).
define native-build-guard
host_goos="$$(go env GOHOSTOS)"; \
host_goarch="$$(go env GOHOSTARCH)"; \
target_goos="$(if $(GOOS),$(GOOS),$$(go env GOOS))"; \
target_goarch="$(if $(GOARCH),$(GOARCH),$$(go env GOARCH))"; \
if [ "$$target_goos" != "linux" ]; then \
	echo "Skipping $(1): requires Linux (target=$$target_goos/$$target_goarch)"; \
	exit 0; \
fi; \
if [ "$$host_goos/$$host_goarch" != "$$target_goos/$$target_goarch" ]; then \
	echo "$(1) cross-build is unsupported (host=$$host_goos/$$host_goarch, target=$$target_goos/$$target_goarch)" >&2; \
	echo "use the execd Docker build for multi-architecture Linux artifacts" >&2; \
	exit 1; \
fi
endef

##@ Development

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

.PHONY: test-integration
test-integration: ## Run integration tests (Linux + bwrap required).
	go test -v -tags="linux,bwrap" -run Integration ./pkg/runtime/bwrap_test/

##@ Linter

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
	"$(GOLANGCI_LINT)" run -v ./...

##@ Native helpers

.PHONY: build-session-gate
build-session-gate:
	@set -eu; \
	$(call native-build-guard,session gate); \
	mkdir -p bin; \
	$(CC) $(CPPFLAGS) $(NATIVE_CFLAGS) "$(SESSION_GATE_SOURCE)" \
		$(NATIVE_LDFLAGS) -o "$(SESSION_GATE_BINARY).tmp"; \
	mv -f "$(SESSION_GATE_BINARY).tmp" "$(SESSION_GATE_BINARY)"

.PHONY: install-session-gate
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
		"$(DESTDIR)$(NATIVE_RUNTIME_DIR)"
	@if [ -z "$(DESTDIR)" ]; then \
		chown root:root \
			"$(SESSION_GATE_SOURCE_INSTALL_DIR)" \
			"$(NATIVE_RUNTIME_DIR)"; \
	fi
	chmod go-w \
		"$(DESTDIR)$(SESSION_GATE_SOURCE_INSTALL_DIR)" \
		"$(DESTDIR)$(NATIVE_RUNTIME_DIR)"
	$(INSTALL) $(NATIVE_INSTALL_OWNER_ARGS) -m 0555 "$(SESSION_GATE_BINARY)" \
		"$(DESTDIR)$(SESSION_GATE_SOURCE_INSTALL_DIR)/opensandbox-session-gate"
	$(INSTALL) $(NATIVE_INSTALL_OWNER_ARGS) -m 0555 "$(SESSION_GATE_BINARY)" \
		"$(DESTDIR)$(NATIVE_RUNTIME_DIR)/opensandbox-session-gate"

.PHONY: build-launcher
build-launcher:
	@set -eu; \
	$(call native-build-guard,launcher); \
	mkdir -p bin; \
	$(CC) $(CPPFLAGS) $(NATIVE_CFLAGS) "$(LAUNCHER_SOURCE)" \
		$(NATIVE_LDFLAGS) -o "$(LAUNCHER_BINARY).tmp"; \
	mv -f "$(LAUNCHER_BINARY).tmp" "$(LAUNCHER_BINARY)"

.PHONY: install-launcher
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
	@umask 022; mkdir -p "$(DESTDIR)$(NATIVE_RUNTIME_DIR)"
	@if [ -z "$(DESTDIR)" ]; then \
		chown root:root "$(DESTDIR)$(NATIVE_RUNTIME_DIR)"; \
	fi
	chmod go-w "$(DESTDIR)$(NATIVE_RUNTIME_DIR)"
	$(INSTALL) $(NATIVE_INSTALL_OWNER_ARGS) -m 0555 "$(LAUNCHER_BINARY)" \
		"$(DESTDIR)$(NATIVE_RUNTIME_DIR)/opensandbox-launcher"

##@ Build

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
	$(MAKE) generate-ebpf ARCH=$(shell go env GOARCH)
	CGO_ENABLED=1 go build -tags ebpf $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/execd-ebpf main.go
	@echo "built bin/execd-ebpf"

# Regenerate the CO-RE audit bytecode for one architecture from
# prog/audit.bpf.c + prog/audit_types.h. The types header declares only the
# kernel members the programs touch (resolved by name against the target
# kernel BTF at load time), so no vmlinux.h is needed and the bytecode is
# hermetic across architectures (issue #1563).
#
#   make generate-ebpf ARCH=amd64   # or arm64; defaults to GOARCH
#
# Requires clang with the bpf target. bpf2go writes audit_bpf_<target>.{go,o};
# the Go build picks the right one via build tags.
.PHONY: generate-ebpf
ARCH ?= $(shell go env GOARCH)
generate-ebpf:
	@case "$(ARCH)" in \
		amd64|arm64) ;; \
		*) echo "generate-ebpf: unsupported ARCH=$(ARCH) (amd64|arm64)" >&2; exit 1 ;; \
	esac
	go run github.com/cilium/ebpf/cmd/bpf2go@v0.16.0 \
		-cc clang -no-strip \
		-cflags "-Ipkg/ebpf/prog" \
		-target $(ARCH) \
		-go-package ebpf -output-dir pkg/ebpf \
		audit pkg/ebpf/prog/audit.bpf.c
	@echo "regenerated pkg/ebpf/audit_bpf_$(ARCH).{go,o}"

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
