# OpenTelemetry Metrics — Ingress

Meter: `opensandbox/ingress` · Service name: `opensandbox-ingress-<version>`

## Metrics

### HTTP requests & routing

| Metric | Type | Unit | Attributes | Description |
|---|---|---|---|---|
| `ingress.http.request.count` | Counter | — | `http_method`, `http_status_code`, `proxy_type` | Request count (QPS derivable) |
| `ingress.http.request.duration` | Histogram | `ms` | `http_method`, `http_status_code`, `proxy_type` | Full request duration |
| `ingress.routing.resolutions.count` | Counter | — | `routing_result` | Routing resolution count |
| `ingress.routing.resolution.duration` | Histogram | `ms` | `routing_result` | Routing resolution latency |

### Upstream connectivity

| Metric | Type | Unit | Attributes | Description |
|---|---|---|---|---|
| `ingress.upstream.connect.count` | Counter | — | `connect_result`, `proxy_type` | Upstream TCP dial attempts by result |
| `ingress.upstream.connect.duration` | Histogram | `ms` | `connect_result`, `proxy_type` | Upstream TCP dial duration |

### Network readiness (shadow assessment)

Reported for the most recent complete window; tuned by the `--network-readiness-shadow-*` flags. Shadow gauges never gate traffic.

| Metric | Type | Unit | Description |
|---|---|---|---|
| `ingress.network.shadow.attempts` | Observable Gauge | — | TCP connection attempts in the window |
| `ingress.network.shadow.signal_failures` | Observable Gauge | — | Timeout or unreachable results in the window |
| `ingress.network.shadow.distinct_targets` | Observable Gauge | — | Bounded distinct upstream targets in the window |
| `ingress.network.shadow.distinct_signal_targets` | Observable Gauge | — | Distinct targets with timeout/unreachable results |
| `ingress.network.shadow.qualified` | Observable Gauge | — | 1 when the window has enough samples |
| `ingress.network.shadow.degraded` | Observable Gauge | — | 1 when the qualified window is degraded |

### Host metrics (Linux only; 0 elsewhere)

| Metric | Type | Unit | Description |
|---|---|---|---|
| `ingress.system.cpu.usage` | Observable Gauge | `1` | CPU busy ratio `[0,1]` |
| `ingress.system.memory.usage_bytes` | Observable Gauge | `By` | Memory used bytes |
| `ingress.connections.active` | Observable Gauge | — | TCP ESTABLISHED connections (from `/proc/net/tcp`) |

**Attribute values:**

- `proxy_type`: `http` | `websocket`
- `routing_result`: `success` | `not_found` | `not_ready` | `error`
- `connect_result`: `success` | `timeout` | `unreachable` | `refused` | `dns_error` | `canceled` | `other`

## Temporality

Default: delta for synchronous Counter/Histogram, cumulative for observable gauges. Override with `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE`.

## Configuration

```bash
export OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://otel-collector:4318"
```

Fallback: `OTEL_EXPORTER_OTLP_ENDPOINT`. When both are unset, export falls back to the node IP (`HOST_IP` env, then `/etc/hostinfo`) on port 4318 over plain HTTP; if no node IP resolves, metrics are not exported.

Disable export with `OTEL_SDK_DISABLED=true` or `OTEL_METRICS_EXPORTER=none`.
