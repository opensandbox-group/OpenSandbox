BINARY := opensandbox-bench
BIN_DIR := bin
LINUX_ARCH ?= amd64
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/version.Version=$(VERSION) \
	-X github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/version.GitCommit=$(GIT_COMMIT) \
	-X github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/version.BuildTime=$(BUILD_TIME)

.PHONY: build build-linux test tidy clean

build:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/opensandbox-bench

build-linux:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(LINUX_ARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY)-linux-$(LINUX_ARCH) ./cmd/opensandbox-bench

test:
	go test -v ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR)
