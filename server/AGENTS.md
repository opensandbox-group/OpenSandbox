# Server AGENTS

OpenSandbox lifecycle server. Routes thin — logic in services/validators/repositories/runtime helpers.

## Scope

`opensandbox_server/**`, `tests/**`, `configuration.md`, `docker-compose.example.yaml`.

Cross-cutting: specs changes → read `../specs/AGENTS.md`; K8s runtime → read `../kubernetes/AGENTS.md`.

## Key Paths

| Path | Role |
|------|------|
| `opensandbox_server/cli.py` | CLI entry point, config init |
| `opensandbox_server/main.py` | FastAPI app entry, startup wiring |
| `opensandbox_server/config.py` | TOML config model, defaults, validation |
| `opensandbox_server/api/` | Routes and request/response schemas |
| `opensandbox_server/services/` | Business logic and runtime integration |
| `opensandbox_server/services/docker/` | Docker runtime, endpoints, ports, diagnostics, snapshots |
| `opensandbox_server/services/k8s/` | K8s providers, templates, informer, egress, pool, pause/resume |
| `opensandbox_server/repositories/` | Persistence backends, snapshot metadata |
| `opensandbox_server/integrations/` | Optional external integrations |
| `opensandbox_server/extensions/` | Extension loading and behavior hooks |
| `opensandbox_server/middleware/` | Auth and request middleware |
| `tests/` | Unit, integration, smoke, K8s tests |

## Commands

```bash
cd server
uv sync --all-groups          # setup
uv run ruff check             # lint
uv run pytest tests/test_docker_service.py   # focused test
uv run pytest tests/k8s       # k8s tests
uv run pyright                # type check
uv run pytest                 # full suite
```

## Guardrails

**Always:** routes thin, runtime-specific logic in docker/k8s modules, snapshot state coordinated across layers, config defaults/examples/docs aligned, extend existing fixtures, regression tests with every fix.

**Ask first:** removing/renaming endpoints, config shape changes, new external deps, snapshot/pause/resume/egress/pool semantics changes, large reorgs.

**Never:** business logic in route handlers, behavior changes without tests, assume Docker-only is safe for K8s paths.
