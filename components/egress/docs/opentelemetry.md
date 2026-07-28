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
| `egress.system.memory.usage_bytes` | Observable Gauge | `By` | **Node** memory used bytes (Linux: gopsutil; non-Linux build: `0`). |
| `egress.system.cpu.utilization` | Observable Gauge | `1` | **Node** CPU busy ratio in `[0,1]` (Linux: gopsutil; non-Linux build: `0`). |
| `egress.process.memory.usage_bytes` | Observable Gauge | `By` | Memory charged to the sidecar's own cgroup. Only present when cgroupfs is readable. |
| `egress.process.cpu.time` | Observable Counter | `s` | CPU seconds consumed by the sidecar's own cgroup. Only present when cgroupfs is readable. |

### `system` vs `process`

They measure different things, and the difference matters because this sidecar runs **per
sandbox**:

- `egress.system.*` comes from gopsutil, i.e. `/proc/meminfo` and `/proc/stat`, which inside
  a container describe the **node**. Every sandbox on a node therefore publishes the same
  figure under its own `sandbox_id`. Do not chart these "by sandbox": the series look
  per-sandbox but are N copies of one node number. Prefer kubelet/cAdvisor or a node
  exporter for node-level data.
- `egress.process.*` is read from the sidecar's own cgroup (v2 `memory.current` and
  `cpu.stat`, falling back to v1 `memory.usage_in_bytes` and `cpuacct.usage`), so it really
  is per sandbox.

`egress.process.cpu.time` is a **cumulative counter of consumed seconds**, not a sampled
ratio: use `rate()` on it. A ratio depends on the exporter's sampling interval, so it cannot
be re-aggregated or compared across differently configured deployments.

Both `process` instruments are **registered only if their cgroup files can be read**. A
runtime that does not expose cgroupfs — a sandbox pod under `secure_runtime`, for instance —
gets no series at all, rather than a flat zero that reads like an idle sidecar.

## Shared Attributes

All egress metrics may include shared attributes:

- `sandbox_id` from `OPENSANDBOX_EGRESS_SANDBOX_ID` (when set). Without it the sidecars of
  different sandboxes export identical attribute sets, so their series collide in the
  backend — which matters most for the per-sandbox `egress.process.*` gauges.
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
