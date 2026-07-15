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

Authentication is enforced when `server.api_key` is set. All API endpoints (except `/health`, `/docs`, `/redoc`) require the `OPEN-SANDBOX-API-KEY` header:

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
| `[docker]` | Docker runtime: `network_mode`, `host_ip`, image registry |
| `[kubernetes]` | Kubernetes runtime: `workload_provider`, `batchsandbox_template_file` |
| `[egress]` | Egress sidecar for `networkPolicy` enforcement |
| `[ingress]` | Ingress gateway configuration |
| `[secure_runtime]` | Secure container runtime (gVisor, Kata, Firecracker) |
| `[store]` | Persistence backend (default: SQLite at `~/.opensandbox/opensandbox.db`) |
| `[renew_intent]` | Auto-renew on access (experimental) |
| `[agent_sandbox]` | Agent sandbox settings for Kubernetes |

For the full configuration reference with all keys and defaults, see the [server configuration.md](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md).

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
- [Network Isolation](/architecture/network-isolation) — Egress policy design
- [Kubernetes Deployment](/kubernetes/) — Kubernetes-specific setup
