# Development Guide

## Setup

```bash
cd OpenSandbox/server
uv sync --all-groups
cp opensandbox_server/examples/example.config.toml ~/.sandbox.toml
# Edit ~/.sandbox.toml as needed
uv run python -m opensandbox_server.main
```

Example dev config:

```toml
[server]
host = "0.0.0.0"
port = 8080
api_key = "your-secret-api-key-change-this"

[log]
level = "DEBUG"

[runtime]
type = "docker"
execd_image = "opensandbox/execd:v1.0.21"

[docker]
network_mode = "bridge"
```

## Testing

Docker daemon required for integration tests.

```bash
uv run pytest                                    # full suite
uv run pytest tests/test_docker_service.py       # single file
uv run pytest tests/k8s                          # k8s tests
uv run ruff check                                # lint
uv run pyright                                   # type check
uv run pytest --cov=opensandbox_server --cov-report=term  # with coverage
```

## Architecture

Layered architecture:

1. **HTTP Layer** — FastAPI routes, request validation, response serialization
2. **Middleware Layer** — authentication, cross-cutting concerns
3. **Service Layer** — business logic abstraction (`sandbox_service.py`, `snapshot_service.py`, etc.)
4. **Runtime Layer** — Docker (`services/docker/`) and Kubernetes (`services/k8s/`) implementations

### Request Flow: Create Sandbox

```
Client → POST /sandboxes
  → Auth Middleware validates API key
  → lifecycle.create_sandbox() receives CreateSandboxRequest
  → sandbox_service.create_sandbox_async(request)
  → Returns 202 Accepted with Pending status immediately
  → Background thread provisions the sandbox
```

Async provisioning avoids blocking API requests during slow operations (image pull, container start). Sandbox stored in pending state first, transitions to running when ready.

### Expiration System

In-memory timer tracking per sandbox. On timeout, sandbox is cleaned up automatically. Timers synchronized via lock for thread safety.

## Docker Runtime

### Network Modes

**Bridge (recommended):** isolated networks, HTTP proxy for routing.
- Endpoint: `http://{server}/route/{sandbox_id}/{port}/path`

**Host:** sandboxes share host network, direct port access. Not recommended — no network isolation.
- Endpoint: `http://{domain}/{sandbox_id}/{port}`

```bash
# Local Docker
export DOCKER_HOST="unix:///var/run/docker.sock"

# Remote Docker
export DOCKER_HOST="ssh://user@remote-host"
```

### Egress Sidecar (bridge + `networkPolicy`)

- Config: set `[egress].image`; sidecar starts only when request carries `networkPolicy`. Requires `network_mode="bridge"`.
- Network: main container shares sidecar netns (`network_mode=container:<sidecar>`); main drops `NET_ADMIN`; sidecar keeps `NET_ADMIN` for iptables/DNS redirect.
- Ports: host port bindings on sidecar; main container labels record mapped ports for endpoint resolution.
- Lifecycle: sidecar cleaned up on create failure / delete / expiration / abnormal recovery; startup removes orphaned sidecars.
- Injection: `OPENSANDBOX_EGRESS_RULES` env passes `networkPolicy` JSON; sidecar image pulled before start.
