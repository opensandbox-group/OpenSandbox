.PHONY: all build vet test lint

PROJECT_GOFLAGS := -trimpath -buildvcs=false
GO_BUILD_FLAGS := $(strip $(GOFLAGS) $(PROJECT_GOFLAGS))
GO_LDFLAGS := $(strip $(LDFLAGS) -buildid= -B none)

all: build vet test

build:
	go build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" ./...

vet:
	go vet ./...

test:
	go test -v ./...

lint:
	@which staticcheck >/dev/null 2>&1 && staticcheck ./... || echo "staticcheck not installed — skipping lint"
