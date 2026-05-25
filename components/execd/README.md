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

- OpenAPI spec: `../../specs/execd-api.yaml`
- Common capability groups:
  - Code execution (`/code`, SSE stream)
  - Session and command execution (`/session`, `/command`)
  - Filesystem operations (`/files`, `/directories`)
  - PTY over WebSocket (`/pty`)
  - Local metrics endpoints (`/metrics`, `/metrics/watch`)

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

### Environment Variables

| Variable | Description |
|---|---|
| `JUPYTER_HOST` | Same as `--jupyter-host` (overridden by explicit flag). |
| `JUPYTER_TOKEN` | Same as `--jupyter-token` (overridden by explicit flag). |
| `EXECD_API_GRACE_SHUTDOWN` | Same as `--graceful-shutdown-timeout`. |
| `EXECD_JUPYTER_IDLE_POLL_INTERVAL` | Same as `--jupyter-idle-poll-interval`. |
| `EXECD_CLONE3_COMPAT` | Linux clone3 compatibility switch (see below). |
| `EXECD_LOG_FILE` | Optional log output file path; default is stdout. |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Preferred OTLP metrics endpoint. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Fallback OTLP endpoint when metrics-specific endpoint is unset. |
| `OPENSANDBOX_ID` | Optional `sandbox_id` metric/resource attribute. |
| `OPENSANDBOX_EXECD_METRICS_EXTRA_ATTRS` | Optional extra metric attrs (`k=v,k2=v2`). |

## Observability

### OpenTelemetry Metrics

OTLP metrics export is enabled when either endpoint is set:

- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`
- `OTEL_EXPORTER_OTLP_ENDPOINT`

Detailed metric list and attributes [opentelemetry.md](./docs/opentelemetry.md).

### Local Metrics Endpoints

- `GET /metrics`: point-in-time host metrics snapshot
- `GET /metrics/watch`: SSE stream (1s cadence)

## Linux clone3 Compatibility

Some sandbox environments fail on `clone3(2)`.  
Set `EXECD_CLONE3_COMPAT` in sandbox env to force fallback behavior:

- `1` / `true` / `yes` / `on`: enable seccomp fallback
- `reexec`: enable fallback and re-exec binary

## License

`execd` is part of OpenSandbox. See `../../LICENSE`.
