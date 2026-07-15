---
title: Server
description: FastAPI-based control plane for managing the lifecycle of containerized sandboxes across Docker and Kubernetes runtimes.
---

# OpenSandbox Server

A production-grade, FastAPI-based service for managing the lifecycle of containerized sandboxes. It acts as the control plane to create, run, monitor, and dispose isolated execution environments across container platforms.

## Features

### Core capabilities
- **Lifecycle APIs**: Standardized REST interfaces for create, start, pause, resume, delete
- **Pluggable runtimes**:
  - **Docker**: Production-ready
  - **Kubernetes**: Production-ready (see [Kubernetes Controller](/kubernetes/) for deployment)
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

::: warning
Metadata keys under the reserved prefix `opensandbox.io/` are system-managed and cannot be supplied by users.
:::

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
# Kubernetes: --example k8s  (deploy the operator / CRDs per kubernetes/ first)
# Locales: docker-zh | k8s-zh  |  omit --example for a schema-only skeleton  |  add --force to overwrite
```

2. Edit the file for your environment. Full reference: [configuration.md](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md) (all keys, defaults, validation, env vars).

   Topics covered there include: Docker `network_mode` / `host_ip` (e.g. server in Docker Compose), `[egress]` when clients send `networkPolicy`, `[ingress]`, `[secure_runtime]`, Kubernetes `workload_provider` / `batchsandbox_template_file`, `[agent_sandbox]`, TTL caps, `[renew_intent]`.
   The server-wide persistence backend is configured under `[store]`; by default OpenSandbox uses a local SQLite database at `~/.opensandbox/opensandbox.db` for server-managed metadata such as snapshot records.

### Run the server

```bash
opensandbox-server
# opensandbox-server --config /path/to/sandbox.toml
```

Listens on `server.host` / `server.port` from your TOML (defaults in [configuration.md](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md)).

**Health check** (adjust host/port if you changed them):

```bash
curl http://127.0.0.1:8080/health
# -> {"status": "healthy"}
```

## API Documentation

Once the server is running, interactive API documentation is available:

- **Swagger UI**: [http://localhost:8080/docs](http://localhost:8080/docs)
- **ReDoc**: [http://localhost:8080/redoc](http://localhost:8080/redoc)

### API Authentication

Authentication is enforced only when `server.api_key` is set. If the value is empty or missing, the middleware skips API Key checks; however startup requires explicit risk acknowledgment. In interactive TTY mode, type `YES` when prompted. In non-interactive environments (Docker/Kubernetes/CI), set `OPENSANDBOX_INSECURE_SERVER=YES` to proceed. For production, always set a non-empty `server.api_key` and send it via the `OPEN-SANDBOX-API-KEY` header.

::: warning
Strongly recommend enabling `server.api_key`. See [security report Issue #750](https://github.com/opensandbox-group/OpenSandbox/issues/750).
:::

All API endpoints (except `/health`, `/docs`, `/redoc`) require authentication via the `OPEN-SANDBOX-API-KEY` header when authentication is enabled:

```bash
curl -H "OPEN-SANDBOX-API-KEY: your-secret-api-key" http://localhost:8080/v1/sandboxes
```

### Example Usage

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

**Other lifecycle calls** (same `OPEN-SANDBOX-API-KEY` header): `GET /v1/sandboxes/{id}`, `POST /v1/sandboxes/{id}/pause`, `POST /v1/sandboxes/{id}/resume`, `GET /v1/sandboxes/{id}/endpoints/{port}` (append `?use_server_proxy=true` when needed), `POST .../renew-expiration`, `DELETE /v1/sandboxes/{id}`. Full request/response shapes: **Swagger UI** above or OpenAPI under [specs/](/api/).

For Kubernetes-backed sandboxes, pause/resume is implemented via `BatchSandbox.spec.pause` and internal `SandboxSnapshot` resources. The externally visible lifecycle transitions are `Running -> Pausing -> Paused -> Resuming -> Running`.

`secureAccess` currently applies only to **Kubernetes** sandboxes exposed through **ingress gateway mode**. Direct endpoint exposure, including non-gateway ingress configurations, is not supported for secured access.

## Architecture

### Component Responsibilities

- **API Layer** (`opensandbox_server/api/`): HTTP request handling, validation, and response formatting
- **Service Layer** (`opensandbox_server/services/`): Business logic for sandbox lifecycle operations
- **Middleware** (`opensandbox_server/middleware/`): Cross-cutting concerns (authentication, logging)
- **Configuration** (`opensandbox_server/config.py`): Centralized configuration management
- **Runtime Implementations**: Platform-specific sandbox orchestration

### Sandbox Lifecycle States

```
       create()
          |
          v
     +---------+
     | Pending |--------------------+
     +----+----+                    |
          |                         |
          | (provisioning)          |
          v                         |
     +---------+    pause()         |
     | Running |---------------+    |
     +----+----+               |    |
          |                    |    |
          |   resume()         |    |
          |   +--------------+ |    |
          |   |              | |    |
          |   v              | |    |
          | +--------+       | |    |
          +-| Paused |-------+ |    |
          | +----+---+         |    |
          |      |             |    |
          |      v             |    |
          |  +----------+      |    |
          |  | Resuming |------+    |
          |  +----------+           |
          |                         |
          | delete() or expire()    |
          v                         |
     +----------+                   |
     | Stopping |                   |
     +----+-----+                   |
          |                         |
          +----------------+--------+
          |                |
          v                v
     +------------+   +--------+
     | Terminated |   | Failed |
     +------------+   +--------+
```

## Experimental Features

Optional experimental behavior; off by default. See release notes before production.

### Auto-Renew on Access

Extends sandbox TTL when traffic is observed (lifecycle proxy and/or ingress + optional Redis queue). Per-sandbox: on create, set `extensions["access.renew.extend.seconds"]` (string integer 300-86400). Clients using the server proxy: request endpoints with `use_server_proxy=true` (REST) or SDK `ConnectionConfig(..., use_server_proxy=True)`.

## Development

### Code Quality

```bash
cd server
uv run ruff check        # Run linter
uv run ruff check --fix  # Auto-fix issues
uv run ruff format       # Format code
```

### Testing

```bash
cd server
uv run pytest                                                              # Run all tests
uv run pytest --cov=opensandbox_server --cov-report=term --cov-fail-under=80  # With coverage
uv run pytest tests/test_docker_service.py::test_create_sandbox_requires_entrypoint  # Specific test
```

## License

This project is licensed under the terms specified in the LICENSE file in the repository root.
