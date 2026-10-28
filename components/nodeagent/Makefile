VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell if [ -n "$$SOURCE_DATE_EPOCH" ]; then date -u -d "@$$SOURCE_DATE_EPOCH" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || date -u -r "$$SOURCE_DATE_EPOCH" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null; else date -u +"%Y-%m-%dT%H:%M:%SZ"; fi)
GO ?= go
CLANG_DEFAULT := $(shell command -v clang-18 2>/dev/null || command -v clang 2>/dev/null)
CLANG ?= $(CLANG_DEFAULT)
LLVM_OBJCOPY_DEFAULT := $(shell command -v llvm-objcopy-18 2>/dev/null || command -v llvm-objcopy 2>/dev/null)
LLVM_OBJCOPY ?= $(LLVM_OBJCOPY_DEFAULT)
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

.PHONY: generate-syscalls-bpf
generate-syscalls-bpf:
	@case "$$(uname -s 2>/dev/null)" in Linux) ;; *) echo "syscall BPF generation requires Linux" >&2; exit 1;; esac
	@case "$$(uname -m 2>/dev/null)" in x86_64) ;; *) echo "syscall BPF generation requires Linux x86_64" >&2; exit 1;; esac
	@command -v "$(CLANG)" >/dev/null 2>&1 || { echo "clang not found: $(CLANG)" >&2; exit 1; }
	@"$(CLANG)" --version | head -n 1 | grep -Eq 'clang version 18\.' || { echo "syscall BPF generation requires clang 18: $(CLANG)" >&2; exit 1; }
	$(CLANG) -target bpfel -O2 -g -fdebug-prefix-map="$(CURDIR)"=. \
		-I/usr/include/$$(uname -m)-linux-gnu \
		-c pkg/source/syscalls/bpf/syscalls.bpf.c \
		-o "$(or $(BPF_OUTPUT),pkg/source/syscalls/syscalls_bpfel.o)"

.PHONY: verify-syscalls-bpf
# Ignore DWARF header checksums while retaining BPF code, relocations, and BTF.
verify-syscalls-bpf:
	@set -e; \
	command -v "$(LLVM_OBJCOPY)" >/dev/null 2>&1 || { echo "llvm-objcopy not found: $(LLVM_OBJCOPY)" >&2; exit 1; }; \
	workdir="$$(mktemp -d)"; trap 'rm -rf "$$workdir"' EXIT; \
	$(MAKE) --no-print-directory generate-syscalls-bpf BPF_OUTPUT="$$workdir/generated.o"; \
	"$(LLVM_OBJCOPY)" --strip-debug pkg/source/syscalls/syscalls_bpfel.o "$$workdir/checked-in.o"; \
	"$(LLVM_OBJCOPY)" --strip-debug "$$workdir/generated.o" "$$workdir/generated-stripped.o"; \
	cmp "$$workdir/checked-in.o" "$$workdir/generated-stripped.o" \
		|| { echo "pkg/source/syscalls/syscalls_bpfel.o is stale; run 'make generate-syscalls-bpf'" >&2; exit 1; }

.PHONY: check
check: fmt-check vet test integration-compile build

.PHONY: clean
clean:
	rm -rf bin
