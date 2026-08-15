# Development Guide (Quick)

## Prerequisites
- Go 1.25+
- Docker (optional, for image build)
- Access to a Kubernetes cluster with BatchSandbox CRD installed.

## Install deps
```bash
cd components/ingress
go mod tidy && go mod vendor
```

## Build & Run
```bash
make build          # binary at bin/router with ldflags version info
./bin/router \
  --namespace <target-namespace> \
  --port 28888 \
  --log-level info
```

## Tests & Lint
```bash
make test           # go test ./...
go vet ./...        # included in make build
```

## Docker (with build args)
```bash
docker build \
  --build-arg VERSION=$(git describe --tags --always --dirty) \
  --build-arg GIT_COMMIT=$(git rev-parse HEAD) \
  --build-arg BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ") \
  -t opensandbox/ingress:dev .
```

## Key Paths
- `main.go` — entrypoint, HTTP routes, provider initialization.
- `pkg/proxy/` — HTTP/WebSocket reverse proxy logic.
- `pkg/sandbox/` — Sandbox provider abstraction and BatchSandbox implementation.
- `version/` — build metadata (ldflags).

## Tips
- Health check: `/status.ok`
- Proxy endpoint: `/` (routes based on `OpenSandbox-Ingress-To` header or Host)
- Env overrides: `VERSION/GIT_COMMIT/BUILD_TIME` usable via Makefile and build.sh.
- BatchSandbox must have `sandbox.opensandbox.io/endpoints` annotation with JSON array of IPs.

