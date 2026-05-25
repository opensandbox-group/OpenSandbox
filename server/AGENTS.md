# Server AGENTS

You are working on the OpenSandbox lifecycle server. Keep the route layer thin and put behavior in services, validators, repositories, or runtime helpers.

## Scope

- `opensandbox_server/**`
- `tests/**`
- `configuration.md`, `docker-compose.example.yaml`, and other server-facing docs/examples

If the task changes lifecycle API contracts in `../specs/sandbox-lifecycle.yml`, also read `../specs/AGENTS.md`.
If the task changes Kubernetes runtime behavior, also read `../kubernetes/AGENTS.md`.

## Key Paths

- `opensandbox_server/cli.py`: `opensandbox-server` CLI entry point and config initialization
- `opensandbox_server/main.py`: FastAPI app entry point and startup wiring
- `opensandbox_server/api/`: FastAPI routes and request/response schemas
- `opensandbox_server/services/`: business logic and runtime integration
- `opensandbox_server/services/docker/`: Docker runtime, endpoints, port allocation, diagnostics, and snapshot runtime
- `opensandbox_server/services/k8s/`: Kubernetes providers, templates, informer, egress, pool, diagnostics, and pause/resume runtime integration
- `opensandbox_server/repositories/`: persistence backends, including snapshot metadata
- `opensandbox_server/integrations/`: optional external integrations
- `opensandbox_server/extensions/`: extension loading and optional behavior hooks
- `opensandbox_server/middleware/`: authentication and request middleware
- `opensandbox_server/config.py`: TOML config model, defaults, validation, and environment integration
- `tests/`: unit, integration, smoke, and Kubernetes-focused tests

## Commands

Setup and focused checks:

```bash
cd server
uv sync --all-groups
uv run ruff check
uv run pytest tests/test_docker_service.py
uv run pytest tests/test_schema.py
```

Kubernetes-focused checks:

```bash
cd server
uv run pytest tests/k8s
uv run pytest tests/test_routes_pause_resume.py tests/test_routes_snapshots.py tests/test_snapshot_service.py
```

Typed or broader validation:

```bash
cd server
uv run pyright
uv run pytest
```

Local startup:

```bash
cd server
uv run opensandbox-server init-config ~/.sandbox.toml --example docker
uv run python -m opensandbox_server.main
```

Smoke path when Docker is available:

```bash
cd server
chmod +x tests/smoke.sh
./tests/smoke.sh
```

## Guardrails

Always:

- Keep FastAPI routes thin and delegate behavior to services, validators, or runtime helpers.
- Keep runtime-specific behavior in Docker/Kubernetes service modules; shared API behavior belongs in common services or validators.
- Keep snapshot state changes coordinated across route handlers, services, repositories, and runtime-specific snapshot implementations.
- Keep TOML config defaults, config examples, README/configuration docs, and CLI `init-config` output aligned.
- Extend existing fixtures and helpers before adding parallel abstractions.
- Add focused regression tests with every bug fix or behavior change.

Ask first:

- Removing or renaming public endpoints
- Changing config shape or defaults in a user-visible way
- Introducing new external service dependencies
- Changing snapshot, pause/resume, renew-intent, ingress, egress, or pool semantics
- Large reorganizations across `api/`, `services/`, and `tests/`

Never:

- Put business logic directly in route handlers.
- Change public server behavior without tests.
- Assume Docker-only behavior is harmless for Kubernetes paths.
