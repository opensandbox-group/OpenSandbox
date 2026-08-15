# OpenTelemetry Metrics (Current Execd Support)

This page lists the OpenTelemetry metrics currently implemented in execd.

## Meter

- `opensandbox/execd`

## Metrics

| Metric | Type | Unit | Attributes | Meaning |
|---|---|---|---|---|
| `execd.http.request.duration` | Histogram | `ms` | `http_method`, `http_route`, `http_status_code` | HTTP request latency. |
| `execd.execution.duration` | Histogram | `ms` | `operation`, `result` | Code/command execution duration. |
| `execd.filesystem.operations.duration` | Histogram | `ms` | `operation`, `result` | Filesystem operation duration. |
| `execd.system.process.count` | Observable Gauge | - | - | Current process count. |
| `execd.system.cpu.usage` | Observable Gauge | `%` | - | System CPU usage percent (gopsutil). |
| `execd.system.memory.usage_bytes` | Observable Gauge | `By` | - | System memory used bytes (gopsutil). |
| `execd.system.network.io.bytes` | Observable Counter | `By` | `direction` (`in`/`out`) | Cumulative network I/O bytes. |
| `execd.system.network.connections.active` | Observable Gauge | - | `protocol` (`tcp`/`udp`) | Current active network connections. |
| `execd.isolation.run.duration` | Histogram | `ms` | `result` | Duration of isolated session runs by result. |
| `execd.isolation.session.count` | Observable Gauge | - | - | Current number of active isolated sessions. |
| `execd.isolation.upper.usage_bytes` | Observable Gauge | `By` | - | Total bytes used by isolated session upper directories. |

## Shared Attributes

All execd metrics may include shared attributes:

- `sandbox_id` from `OPENSANDBOX_ID` (when set)
- extra key/value attributes from `OPENSANDBOX_EXECD_METRICS_EXTRA_ATTRS` (when set)

## OTEL Endpoint Configuration

Metric export is enabled when an OTLP endpoint is set, or (unless explicitly disabled) when a node IP is available via `HOST_IP` or `/etc/hostinfo`, in which case metrics are exported insecurely to `<ip>:4318`.

- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` (preferred)
- `OTEL_EXPORTER_OTLP_ENDPOINT` (fallback)
- `HOST_IP` (fallback endpoint host when both OTLP env vars are unset)

### Minimal Example

```bash
export OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://otel-collector:4318"
```

### Optional Service Name

```bash
export OTEL_SERVICE_NAME="opensandbox-execd"
```
