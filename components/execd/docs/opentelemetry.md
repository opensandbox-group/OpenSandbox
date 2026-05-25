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

## Shared Attributes

All execd metrics may include shared attributes:

- `sandbox_id` from `OPENSANDBOX_ID` (when set)
- extra key/value attributes from `OPENSANDBOX_EXECD_METRICS_EXTRA_ATTRS` (when set)

## OTEL Endpoint Configuration

Metric export is enabled only when at least one OTLP endpoint is set.

- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` (preferred)
- `OTEL_EXPORTER_OTLP_ENDPOINT` (fallback)

If both are unset, execd keeps metrics local (no OTLP export).

### Minimal Example

```bash
export OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://otel-collector:4318"
```

### Optional Service Name

```bash
export OTEL_SERVICE_NAME="opensandbox-execd"
```
