# OpenSandbox Server

A production-grade, FastAPI-based service for managing the lifecycle of containerized sandboxes. It acts as the control plane to create, run, monitor, and dispose isolated execution environments across container platforms.

## Features

### Core capabilities
- **Lifecycle APIs**: Standardized REST interfaces for create, start, pause, resume, delete
- **Pluggable runtimes**:
  - **Docker**: Production-ready
  - **Kubernetes**: Production-ready (see [`../kubernetes/README.md`](../kubernetes/README.md) for deployment)
- **Lifecycle cleanup modes**: Configurable TTL with renewal, or manual cleanup with explicit delete
- **Access control**: API Key authentication (`OPEN-SANDBOX-API-KEY`); can be disabled for local/dev
- **Networking modes**:
  - Host: shared host network, performance first
  - Bridge: isolated network with built-in HTTP routing
- **Resource quotas**: CPU/memory limits with Kubernetes-style specs
- **Observability**: Unified status with transition tracking
- **Registry support**: Public and private images

### Extended capabilities
- **Async provisioning**: Background creation to reduce latency
- **Timer restoration**: Expiration timers restored after restart
- **Env/metadata injection**: Per-sandbox environment and metadata
- **Port resolution**: Dynamic endpoint generation
- **Structured errors**: Standard error codes and messages

Metadata keys under the reserved prefix `opensandbox.io/` are system-managed
and cannot be supplied by users.

## Requirements

- **Python**: 3.10 or higher
- **Package Manager**: [uv](https://github.com/astral-sh/uv) (recommended) or pip
- **Runtime Backend**:
  - Docker Engine 20.10+ (for Docker runtime)
  - Kubernetes 1.21.1+ (for Kubernetes runtime)
- **Operating System**: Linux, macOS, or Windows with WSL2

## Quick Start

### Installation

Install from PyPI. For local development, clone the repo and run `uv sync` in `server/`.

```bash
uv pip install opensandbox-server
```

### Configuration

The server reads a **TOML** file. Default path: `~/.sandbox.toml`. Override with **`SANDBOX_CONFIG_PATH`** or **`opensandbox-server --config /path/to/sandbox.toml`**.

1. Generate a starter file (see `opensandbox-server -h` for all flags):

```bash
opensandbox-server init-config ~/.sandbox.toml --example docker
# Kubernetes: --example k8s  (deploy the operator / CRDs per ../kubernetes/ first)
# Locales: docker-zh | k8s-zh  |  omit --example for a schema-only skeleton  |  add --force to overwrite
```

2. Edit the file for your environment. **Full reference:** **[configuration.md](configuration.md)** (all keys, defaults, validation, env vars).

   Topics covered there include: Docker **`network_mode`** / **`host_ip`** (e.g. server in Docker Compose), **`[egress]`** when clients send **`networkPolicy`**, **`[ingress]`**, **`[secure_runtime]`**, Kubernetes **`workload_provider`** / **`batchsandbox_template_file`**, **`[agent_sandbox]`**, TTL caps, **`[renew_intent]`**.
   The server-wide persistence backend is configured under **`[store]`**; by default OpenSandbox uses a local SQLite database at `~/.opensandbox/opensandbox.db` for server-managed metadata such as snapshot records.

**Also useful:** [Secure container runtime](../docs/secure-container.md) · [Manual cleanup / optional fields](../docs/manual-cleanup-refactor-guide.md) · [Egress component](../components/egress/README.md) · [`docker-compose.example.yaml`](docker-compose.example.yaml) · [Experimental features](#experimental-features)

### Run the server

```bash
opensandbox-server
# opensandbox-server --config /path/to/sandbox.toml
```

Listens on `server.host` / `server.port` from your TOML (defaults in [configuration.md](configuration.md)).

**Health check** (adjust host/port if you changed them):

```bash
curl http://127.0.0.1:8080/health
# → {"status": "healthy"}
```

If startup, Docker/Kubernetes, or connectivity fails, see **[Troubleshooting](TROUBLESHOOTING.md)**.

## API documentation

Once the server is running, interactive API documentation is available:

- **Swagger UI**: [http://localhost:8080/docs](http://localhost:8080/docs)
- **ReDoc**: [http://localhost:8080/redoc](http://localhost:8080/redoc)

### API authentication

Authentication is enforced only when `server.api_key` is set. If the value is empty or missing, the middleware skips API Key checks; however startup requires explicit risk acknowledgment. In interactive TTY mode, type `YES` when prompted. In non-interactive environments (Docker/Kubernetes/CI), set `OPENSANDBOX_INSECURE_SERVER=YES` to proceed. For production, always set a non-empty `server.api_key` and send it via the `OPEN-SANDBOX-API-KEY` header.

**Strongly recommend enabling `server.api_key`; see security report [Issue #750](https://github.com/alibaba/OpenSandbox/issues/750)**.

All API endpoints (except `/health`, `/docs`, `/redoc`) require authentication via the `OPEN-SANDBOX-API-KEY` header when authentication is enabled:

```bash
curl -H "OPEN-SANDBOX-API-KEY: your-secret-api-key" http://localhost:8080/v1/sandboxes
```

### Example usage

**Create a Sandbox**

```bash
curl -X POST "http://localhost:8080/v1/sandboxes" \
  -H "OPEN-SANDBOX-API-KEY: your-secret-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "image": {
      "uri": "python:3.11-slim"
    },
    "entrypoint": [
      "python",
      "-m",
      "http.server",
      "8000"
    ],
    "timeout": 3600,
    "resourceLimits": {
      "cpu": "500m",
      "memory": "512Mi"
    },
    "env": {
      "PYTHONUNBUFFERED": "1"
    },
    "metadata": {
      "team": "backend",
      "project": "api-testing"
    }
  }'
```

Response:
```json
{
  "id": "a1b2c3d4-5678-90ab-cdef-1234567890ab",
  "status": {
    "state": "Pending",
    "reason": "CONTAINER_STARTING",
    "message": "Sandbox container is starting.",
    "lastTransitionAt": "2024-01-15T10:30:00Z"
  },
  "metadata": {
    "team": "backend",
    "project": "api-testing"
  },
  "expiresAt": "2024-01-15T11:30:00Z",
  "createdAt": "2024-01-15T10:30:00Z",
  "entrypoint": ["python", "-m", "http.server", "8000"]
}
```

**Other lifecycle calls** (same `OPEN-SANDBOX-API-KEY` header): `GET /v1/sandboxes/{id}`, `POST /v1/sandboxes/{id}/pause`, `POST /v1/sandboxes/{id}/resume`, `GET /v1/sandboxes/{id}/endpoints/{port}` (append `?use_server_proxy=true` when needed), `POST .../renew-expiration`, `DELETE /v1/sandboxes/{id}`. Full request/response shapes: **Swagger UI** above or OpenAPI under [`specs/`](../specs/).

For Kubernetes-backed sandboxes, pause/resume is implemented via `BatchSandbox.spec.pause` and internal `SandboxSnapshot` resources. The externally visible lifecycle transitions are `Running -> Pausing -> Paused -> Resuming -> Running`. Operational details are documented in [docs/pause-resume.md](../docs/pause-resume.md).

`secureAccess` currently applies only to **Kubernetes** sandboxes exposed through **ingress gateway mode**. Direct endpoint exposure, including non-gateway ingress configurations, is not supported for secured access.

## Architecture

### Component responsibilities

- **API Layer** (`opensandbox_server/api/`): HTTP request handling, validation, and response formatting
- **Service Layer** (`opensandbox_server/services/`): Business logic for sandbox lifecycle operations
- **Middleware** (`opensandbox_server/middleware/`): Cross-cutting concerns (authentication, logging)
- **Configuration** (`opensandbox_server/config.py`): Centralized configuration management
- **Runtime Implementations**: Platform-specific sandbox orchestration

### Sandbox lifecycle states

```
       create()
          │
          ▼
     ┌─────────┐
     │ Pending │────────────────────┐
     └────┬────┘                    │
          │                         │
          │ (provisioning)          │
          ▼                         │
     ┌─────────┐    pause()         │
     │ Running │───────────────┐    │
     └────┬────┘               │    │
          │                    │    │
          │   resume()         │    │
          │   ┌──────────────┐ │    │
          │   │              │ │    │
          │   ▼              │ │    │
          │ ┌────────┐       │ │    │
          ├─│ Paused │───────┘ │    │
          │ └────┬───┘         │    │
          │      │             │    │
          │      ▼             │    │
          │  ┌──────────┐      │    │
          │  │ Resuming │──────┘    │
          │  └──────────┘           │
          │                         │
          │ delete() or expire()    │
          ▼                         │
     ┌──────────┐                   │
     │ Stopping │                   │
     └────┬─────┘                   │
          │                         │
          ├────────────────┬────────┘
          │                │
          ▼                ▼
     ┌────────────┐   ┌────────┐
     │ Terminated │   │ Failed │
     └────────────┘   └────────┘
```

## Configuration reference

Single source of truth for TOML: **[configuration.md](configuration.md)** (includes `SANDBOX_CONFIG_PATH`, `DOCKER_HOST`, `PENDING_FAILURE_TTL`).

## Experimental features

Optional **🧪 experimental** behavior; **off by default** in [`example.config.toml`](opensandbox_server/examples/example.config.toml). See release notes before production.

### Auto-renew on access

Extends sandbox TTL when traffic is observed (lifecycle **proxy** and/or **ingress** + optional **Redis** queue). Design and operations: **[OSEP-0009](../oseps/0009-auto-renew-sandbox-on-ingress-access.md)**. TOML keys (`[renew_intent]`, including nested `redis.*`): see **[configuration.md](configuration.md)** and [`example.config.toml`](opensandbox_server/examples/example.config.toml).

Per-sandbox: on **create**, set `extensions["access.renew.extend.seconds"]` (string integer **300**–**86400**). Clients using the server proxy: request endpoints with `use_server_proxy=true` (REST) or SDK `ConnectionConfig(..., use_server_proxy=True)` — details in OSEP-0009.

## Development

### Code quality

**Run linter**:
```bash
uv run ruff check
```

**Auto-fix issues**:
```bash
uv run ruff check --fix
```

**Format code**:
```bash
uv run ruff format
```

### Testing

**Run all tests**:
```bash
uv run pytest
```

**Run with coverage**:
```bash
uv run pytest --cov=opensandbox_server --cov-report=term --cov-fail-under=80
```

**Run specific test**:
```bash
uv run pytest tests/test_docker_service.py::test_create_sandbox_requires_entrypoint
```

## License

This project is licensed under the terms specified in the LICENSE file in the repository root.

## Contributing

Contributions are welcome. Follow the repository **[CONTRIBUTING.md](../CONTRIBUTING.md)** (Conventional Commits, PR expectations). Typical flow:

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Write tests for new functionality
4. Ensure all tests pass (`uv run pytest`)
5. Run linting (`uv run ruff check`)
6. Commit with clear messages
7. Push to your fork
8. Open a Pull Request

## Support

- **Troubleshooting:** [TROUBLESHOOTING.md](TROUBLESHOOTING.md) — common failures (config, Docker, networking, K8s) and fixes
- **Development:** [DEVELOPMENT.md](DEVELOPMENT.md)
- **Issues:** Report defects via GitHub Issues
- **Discussions:** GitHub Discussions for Q&A and ideas
