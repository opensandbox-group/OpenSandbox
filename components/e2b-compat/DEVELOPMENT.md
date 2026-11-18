# Development Guide (Quick)

## Prerequisites

- Go 1.25+

## Build & Run

```bash
make build          # binary at bin/e2b-compat with ldflags version info
./bin/e2b-compat --help
./bin/e2b-compat adapter --listen 127.0.0.1:8080
./build.sh          # same local build without make
```

## Tests & Lint

```bash
make test           # go vet && go test ./...
make race           # go test -race ./...
go vet ./...
make golint         # golangci-lint (install-golint target included)
```
