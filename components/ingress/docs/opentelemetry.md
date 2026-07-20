# OpenTelemetry Metrics — Ingress

Meter: `opensandbox/ingress` · Service name: `opensandbox-ingress-<version>`

## Metrics

| Metric | Type | Unit | Attributes | Description |
|---|---|---|---|---|
| `ingress.http.request.count` | Counter | — | `http_method`, `http_status_code`, `proxy_type` | Request count (QPS derivable) |
| `ingress.http.request.duration` | Histogram | `ms` | `http_method`, `http_status_code`, `proxy_type` | Full request duration |
| `ingress.routing.resolutions.count` | Counter | — | `routing_result` | Routing resolution count |
| `ingress.routing.resolution.duration` | Histogram | `ms` | `routing_result` | Routing resolution latency |
| `ingress.connections.active` | Observable Gauge | — | — | TCP ESTABLISHED connections (from `/proc/net/tcp`) |
| `ingress.system.cpu.usage` | Observable Gauge | `1` | — | CPU busy ratio `[0,1]` (Linux only) |
| `ingress.system.memory.usage_bytes` | Observable Gauge | `By` | — | Memory used bytes (Linux only) |

**Attribute values:** `proxy_type`: `http` | `websocket`. `routing_result`: `success` | `not_found` | `not_ready` | `error`.

**Temporality:** Delta.

## Configuration

```bash
export OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://otel-collector:4318"
```

Fallback: `OTEL_EXPORTER_OTLP_ENDPOINT`. Both unset = no export.
