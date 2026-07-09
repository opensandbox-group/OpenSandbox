---
title: execd
description: The in-sandbox execution daemon providing HTTP APIs for code execution, shell commands, filesystem operations, PTY sessions, and metrics.
---

# execd - OpenSandbox Execution Daemon

`execd` is the runtime daemon used inside OpenSandbox sandboxes.

It is built on Gin and exposes HTTP APIs for code execution, shell commands, filesystem operations, PTY sessions, and metrics.

## Quick Start

### 1) Build

```bash
cd components/execd
make build
```

### 2) Start Jupyter Server

```bash
./tests/jupyter.sh
```

### 3) Run execd

```bash
./bin/execd \
  --jupyter-host=http://127.0.0.1:54321 \
  --jupyter-token=your-jupyter-token \
  --port=44772
```

### 4) Verify

```bash
curl -v http://localhost:44772/ping
```

## API

- OpenAPI spec: [execd-api.yaml](/api/)
- Common capability groups:
  - Code execution (`/code`, SSE stream)
  - Session and command execution (`/session`, `/command`)
  - Filesystem operations (`/files`, `/directories`)
  - Isolated sessions (`/v1/isolated/session`, bubblewrap namespaces)
  - PTY over WebSocket (`/pty`)
  - Local metrics endpoints (`/metrics`, `/metrics/watch`)

## Isolated Sessions

Isolated sessions run a bash process inside a per-execution
[bubblewrap](https://github.com/containers/bubblewrap) (`bwrap`) namespace,
created via `POST /v1/isolated/session`. Beyond the workspace, callers can
expose additional host paths into the namespace.

### Bind mounts

Two request fields control extra host paths:

- `extra_writable`: a list of paths bind-mounted read-write at the same path
  inside the namespace (`source == destination`).
- `binds`: explicit `source` → `dest` mappings, each optionally read-only.
  - `source` (required): host path to bind. It must **already exist** and is
    resolved (symlinks followed) before use.
  - `dest`: mount destination inside the namespace; defaults to `source` when
    omitted. It must be an **existing** mount point — `bwrap` cannot create a
    destination under the read-only root, so create the directory first.
  - `readonly` (default `false`): mount read-only (`--ro-bind`) when `true`,
    read-write (`--bind`) otherwise.

Example:

```json
{
  "workspace": { "path": "/workspace", "mode": "rw" },
  "binds": [
    { "source": "/data/in",  "dest": "/mnt/in", "readonly": true },
    { "source": "/data/out", "dest": "/mnt/out" }
  ]
}
```

### Writable allowlist

The source path of every `extra_writable` entry and every `binds` entry must
fall within the `allowed_writable` allowlist (see the isolation config file
below). The allowlist is enforced against the fully symlink-resolved real
path, so a symlink cannot redirect a bind outside the allowlist. An empty
allowlist rejects all `extra_writable`/`binds` requests.

The built-in default allowlist is `/workspace`, `/mnt`, `/media`, `/data`
(subpaths included). Set `allowed_writable` in the isolation config to
override it.

## Configuration

### CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--jupyter-host` | `""` | Jupyter server URL reachable by execd. |
| `--jupyter-token` | `""` | Jupyter token for HTTP/WebSocket auth. |
| `--port` | `44772` | HTTP listen port. |
| `--log-level` | `6` | Log level (0=Emergency, 7=Debug). |
| `--access-token` | `""` | Optional shared API access token. |
| `--graceful-shutdown-timeout` | `1s` | SSE tail-drain wait window before closing. |
| `--jupyter-idle-poll-interval` | `100ms` | Poll interval after Jupyter reports idle. |
| `--isolation-config` | `""` | Path to the isolation TOML config (see below). |

### Environment Variables

| Variable | Description |
|---|---|
| `JUPYTER_HOST` | Same as `--jupyter-host` (overridden by explicit flag). |
| `JUPYTER_TOKEN` | Same as `--jupyter-token` (overridden by explicit flag). |
| `EXECD_ACCESS_TOKEN` | Same as `--access-token` (overridden by explicit flag). |
| `EXECD_API_GRACE_SHUTDOWN` | Same as `--graceful-shutdown-timeout`. |
| `EXECD_JUPYTER_IDLE_POLL_INTERVAL` | Same as `--jupyter-idle-poll-interval`. |
| `EXECD_ISOLATION_CONFIG` | Same as `--isolation-config`. |
| `EXECD_CLONE3_COMPAT` | Linux clone3 compatibility switch (see below). |
| `EXECD_LOG_FILE` | Optional log output file path; default is stdout. |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Preferred OTLP metrics endpoint. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Fallback OTLP endpoint when metrics-specific endpoint is unset. |
| `OPENSANDBOX_ID` | Optional `sandbox_id` metric/resource attribute. |
| `OPENSANDBOX_EXECD_METRICS_EXTRA_ATTRS` | Optional extra metric attrs (`k=v,k2=v2`). |

### Isolation Config File

Isolated sessions read an optional TOML file given by `--isolation-config`
(or `EXECD_ISOLATION_CONFIG`). All fields are optional; omitted fields use
built-in defaults.

```toml
# Parent directory for per-session overlay upper directories.
upper_root = "/var/lib/execd/isolation"

# Host paths callers may request via extra_writable / binds.
# Enforced against the fully symlink-resolved real path; subpaths are allowed.
# Default: ["/workspace", "/mnt", "/media", "/data"]. Empty = reject all.
allowed_writable = ["/workspace", "/mnt", "/media", "/data"]
```

## Observability

### OpenTelemetry Metrics

OTLP metrics export is enabled when either endpoint is set:

- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`
- `OTEL_EXPORTER_OTLP_ENDPOINT`

### Local Metrics Endpoints

- `GET /metrics`: point-in-time host metrics snapshot
- `GET /metrics/watch`: SSE stream (1s cadence)

## Linux clone3 Compatibility

Some sandbox environments fail on `clone3(2)`.
Set `EXECD_CLONE3_COMPAT` in sandbox env to force fallback behavior:

- `1` / `true` / `yes` / `on`: enable seccomp fallback
- `reexec`: enable fallback and re-exec binary

## License

`execd` is part of OpenSandbox. See the [LICENSE](https://github.com/opensandbox-group/OpenSandbox/blob/main/LICENSE).
