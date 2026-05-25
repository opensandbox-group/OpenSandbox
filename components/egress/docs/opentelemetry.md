# OpenTelemetry Metrics (Current Egress Support)

This page lists the OpenTelemetry metrics currently implemented in egress.

## Meter

- `opensandbox/egress`

## Metrics

| Metric | Type | Unit | Meaning |
|---|---|---|---|
| `egress.dns.query.duration` | Histogram | `s` | Upstream DNS forward latency (recorded for allowed queries). |
| `egress.policy.denied_total` | Counter | - | Number of DNS queries denied by policy. |
| `egress.nftables.rules.count` | Observable Gauge | `{element}` | Approximate policy size after last successful static apply. |
| `egress.nftables.updates.count` | Counter | - | Number of successful nftables updates (static apply + dynamic IP add). |
| `egress.system.memory.usage_bytes` | Observable Gauge | `By` | System memory used bytes (Linux: gopsutil; non-Linux build: `0`). |
| `egress.system.cpu.utilization` | Observable Gauge | `1` | CPU busy ratio in `[0,1]` (Linux: gopsutil; non-Linux build: `0`). |

## Shared Attributes

All egress metrics may include shared attributes:

- `sandbox_id` from `OPENSANDBOX_EGRESS_SANDBOX_ID` (when set)
- extra key/value attributes from `OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS` (when set)

## OTEL Endpoint Configuration

Metric export is enabled only when at least one OTLP endpoint is set.

- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` (preferred)
- `OTEL_EXPORTER_OTLP_ENDPOINT` (fallback)

If both are unset, egress keeps metrics local (no OTLP export).

### Minimal Example

```bash
export OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://otel-collector:4318"
```

### Service Name

`service.name` is set by egress code as `opensandbox-egress-<version>`.

## Structured logs (JSON)

Egress structured logs are emitted by zap (typically to stdout). OTLP log export is not implemented in-tree.

### Common fields

- `sandbox_id` is included when `OPENSANDBOX_EGRESS_SANDBOX_ID` is set.
- key/value pairs from `OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS` are merged into the root logger.
- `opensandbox.event` identifies the event family.

### Outbound DNS logs

- `opensandbox.event=egress.outbound`
- emitted on allow-path DNS handling (success or forward error)
- common payload keys:
  - `target.host` (normalized query name)
  - `target.ips` (resolved A/AAAA addresses, when present)
  - `peer` (IP-only destination path)
  - `error` (forward failure message)

### Policy lifecycle logs

- `opensandbox.event=egress.loaded` (initial effective policy loaded)
- `opensandbox.event=egress.updated` (policy update applied)
- `opensandbox.event=egress.update_failed` (policy update failed)

Common policy fields:

- `egress.default` (`allow` / `deny`)
- `rules` (rule summary; for `egress.updated`, reflects current request body semantics)
- `error` (present for `egress.update_failed`)
