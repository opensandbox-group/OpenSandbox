# Development Guide (Quick)

## Prerequisites
- Go 1.26+
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
- `pkg/flag/` — command-line flags and defaults.
- `pkg/proxy/` — routing, HTTP/WebSocket forwarding, and wake replay; transport and response-writer details stay package-private.
- `pkg/connectivity/` — TCP observations and network-readiness assessment, independent of proxy routing.
- `pkg/sandbox/` — sandbox provider abstraction (BatchSandbox, AgentSandbox, Fast Sandbox/FastPath).
- `pkg/signature/`, `pkg/routescope/` — signed-route and Fast Sandbox route-scope verification.
- `pkg/renewintent/` — renew-intent event publishing to Redis.
- `pkg/activity/` — asynchronous Redis activity observations.
- `pkg/wake/` — bounded wake-on-access orchestration.
- `pkg/idlepolicy/` — Sandbox CR idle-policy validation.
- `pkg/telemetry/` — OpenTelemetry metrics.
- `vendor/github.com/alibaba/opensandbox/internal/version` — build metadata (ldflags).

## Tips
- Health check: `/status.ok`
- Proxy endpoint: `/` (routes based on `OpenSandbox-Ingress-To` header or Host)
- Env overrides: `VERSION/GIT_COMMIT/BUILD_TIME` usable via Makefile and build.sh.
- BatchSandbox must have `sandbox.opensandbox.io/endpoints` annotation with JSON array of IPs.

