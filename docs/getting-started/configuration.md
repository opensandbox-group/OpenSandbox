---
title: Configuration
description: Server configuration reference for OpenSandbox, covering TOML config, runtimes, networking, and authentication.
---

# Configuration

The OpenSandbox server reads a **TOML** configuration file. Default path: `~/.sandbox.toml`. Override with the `SANDBOX_CONFIG_PATH` environment variable or the `--config` flag.

## Generate a Config File

```bash
# Docker runtime (default)
opensandbox-server init-config ~/.sandbox.toml --example docker

# Kubernetes runtime
opensandbox-server init-config ~/.sandbox.toml --example k8s

# Schema-only skeleton (no defaults)
opensandbox-server init-config ~/.sandbox.toml

# Overwrite existing config
opensandbox-server init-config ~/.sandbox.toml --example docker --force
```

## Run the Server

```bash
opensandbox-server
# or specify a config path
opensandbox-server --config /path/to/sandbox.toml
```

The server listens on the `server.host` / `server.port` values from your TOML config.

## Authentication

Authentication is enforced when `server.api_key` is set. All API endpoints (except `/health`, `/version`, `/docs`, `/redoc`, and `/openapi.json`) require the `OPEN-SANDBOX-API-KEY` header:

```bash
curl -H "OPEN-SANDBOX-API-KEY: your-secret-api-key" http://localhost:8080/v1/sandboxes
```

::: warning
If `server.api_key` is empty, the server runs without authentication. In non-interactive environments (Docker/Kubernetes/CI), set `OPENSANDBOX_INSECURE_SERVER=YES` to acknowledge the risk. **Always set an API key in production.**
:::

## Key Configuration Areas

| Section | Description |
|---------|-------------|
| `[server]` | Host, port, API key, and general server settings |
| `[runtime]` | Runtime selection and execd image configuration |
| `[otel]` | OpenTelemetry export for SDK create-latency metrics |
| `[proxy]` | Server-side sandbox proxy target selection; use host mappings when the server cannot route to sandbox IPs |
| `[docker]` | Docker runtime networking, host address, and image-pull settings |
| `[kubernetes]` | Kubernetes runtime: `workload_provider`, `batchsandbox_template_file` |
| `[egress]` | Egress sidecar for `networkPolicy` enforcement |
| `[ingress]` | Ingress gateway configuration |
| `[secure_runtime]` | Secure container runtime (gVisor, Kata, Firecracker) |
| `[store]` | Persistence backend (default: SQLite at `~/.opensandbox/opensandbox.db`) |
| `[renew_intent]` | Auto-renew on access (experimental) |
| `[agent_sandbox]` | Agent sandbox settings for Kubernetes |

For the full configuration reference with all keys and defaults, see the [server configuration.md](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md).

## Kubernetes Creation Wait

For the BatchSandbox and agent-sandbox providers, the server waits for the workload
status to become `Running` or `Allocated` before returning a successful create
response. This does not check whether your application inside the sandbox is ready.

Creation requests share the existing resource watches. A workload change wakes
its waiting request immediately, which evaluates the object carried by the event
without another API read. The initial check and periodic fallback still use normal
cache/API reads. Events overlapping a read or describing deletion trigger a fresh
check. Providers without subscription support continue polling.

| `[kubernetes]` setting | Default | Meaning |
|------------------------|---------|---------|
| `sandbox_create_timeout_seconds` | `60` | Overall creation wait limit |
| `pool_acquisition_timeout_seconds` | `30` | Cumulative wait limit while Pool capacity is exhausted |
| `sandbox_create_poll_interval_seconds` | `1.0` | Fallback status-check interval when no notification arrives |

The default fallback interval remains unchanged. Increasing it (for example to
`5.0`) reduces repeated checks while nothing changes, but can delay detection
when watch notifications are unavailable. Overall and Pool capacity deadlines
still apply independently of this interval. Unavailable cache reads continue to
fall back to the Kubernetes API.

## API Documentation

Once the server is running, interactive API docs are available at:

- **Swagger UI**: `http://localhost:8080/docs`
- **ReDoc**: `http://localhost:8080/redoc`

## Environment Variables

| Variable | Description |
|----------|-------------|
| `SANDBOX_CONFIG_PATH` | Override the config file path |
| `DOCKER_HOST` | Custom Docker daemon address |
| `OPENSANDBOX_INSECURE_SERVER` | Set to `YES` to run without API key in non-interactive mode |

## Related

- [Secure Container Runtime](/guides/secure-container) — gVisor, Kata, and Firecracker configuration
- [Credential Vault](/guides/credential-vault) — Secure credential injection
- [Network Isolation](/architecture/network/network-isolation) — Egress policy design
- [Kubernetes Deployment](/deployment/) — Kubernetes-specific setup
