---
title: Telemetry
description: OpenTelemetry metrics reference for every OpenSandbox component, with per-metric meanings, attributes, and export configuration.
---

# Telemetry

OpenSandbox components export OpenTelemetry (OTLP/HTTP) metrics. This page is the unified reference: each component's metrics with their meaning and attributes, plus the configuration that controls export. SDK-side creation metrics and pool warmup tracing are covered separately in [SDK observability](/sdks/observability).

| Component | Meter / service name | Metric families |
| --- | --- | --- |
| [Lifecycle server](#lifecycle-server) | `opensandbox-server` | HTTP request latency, SDK-reported create latency |
| [Kubernetes controller](#kubernetes-controller) | `opensandbox/controller` | Allocator histograms, capacity gauges (+ controller-runtime endpoint) |
| [Execd](#execd) | `opensandbox/execd` | HTTP, execution, filesystem, system resources, OOM, isolation |
| [Egress sidecar](#egress-sidecar) | `opensandbox/egress` | DNS, policy, nftables, node/process resources |
| [Ingress gateway](#ingress-gateway) | `opensandbox-ingress-<version>` | HTTP/routing, upstream dial, network readiness shadow, wake-on-access, host |
| [Node agent](#node-agent) | `opensandbox.nodeagent.*` | Log pipeline records, bytes, drops, retries, queue depth, sink latency |

## Shared OTLP configuration for Go components

Execd, egress, ingress, and node-agent share the OTLP metrics exporter in `components/internal/telemetry`. Set these variables in the **component process environment**. Setting them on the lifecycle server or in a sandbox creation request does not automatically configure another container, such as the egress sidecar. For server-managed egress endpoints, see [egress observability](/architecture/network/egress#observability-opentelemetry).

### Export endpoints

- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` takes precedence over `OTEL_EXPORTER_OTLP_ENDPOINT`.
- Export uses HTTP/protobuf against a collector's HTTP receiver (usually port 4318). gRPC export and switching transport with `OTEL_EXPORTER_OTLP_PROTOCOL` are not implemented.
- Node-IP fallback: when neither endpoint is set, execd, egress, and ingress fall back to `HOST_IP:4318`, or the node IP read from `/etc/hostinfo`, over plain HTTP. If no node IP resolves, metrics are not exported. Components that set `DisableEndpointFallback` in their shared telemetry configuration (node-agent) skip this fallback. Consequently, clearing the endpoint variables alone does not always disable metrics.

### Disabling metrics

Set either of these standard switches to prevent metrics export, including explicit endpoints and the node-IP fallback:

```sh
OTEL_SDK_DISABLED=true
# Or disable metrics only:
OTEL_METRICS_EXPORTER=none
```

Values are case-insensitive. Other values leave the existing export configuration unchanged; `OTEL_METRICS_EXPORTER` does not select an alternative exporter here. These switches do not disable native application logs or execd's local `/metrics` and `/metrics/watch` APIs. They also suppress the egress sidecar's automatic collector allow rule, including when the effective policy is rebuilt. Operator-provided allow rules remain in effect.

### Aggregation temporality

Set `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE` to select the exporter's standard aggregation behavior:

| Value | Counter and Histogram | ObservableCounter | UpDownCounter and ObservableUpDownCounter |
|---|---|---|---|
| `cumulative` | Cumulative | Cumulative | Cumulative |
| `delta` | Delta | Delta | Cumulative |
| `lowmemory` | Delta | Cumulative | Cumulative |
| Unset, empty, or whitespace-only | Delta | Cumulative | Cumulative |

Values are case-insensitive. An invalid nonempty value uses the upstream exporter's cumulative default and produces its configuration warning. Gauges remain instantaneous measurements.

Prometheus-style backends expect cumulative counters and histograms. Either set `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=cumulative` in the component environment, or configure the Collector's `deltatocumulative` processor when ingesting delta metrics. Choose the setting before component startup; these environment variables are not hot-reloaded.

## Lifecycle server

Optional OTLP export for server HTTP requests and SDK-reported sandbox creation latency (`POST /v1/metrics/events`). Off by default; the HTTP middleware and the ingestion endpoint remain active but record as noops.

Configuration keys under `[otel]` (see the [server configuration reference](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md) for the authoritative list):

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | boolean | `false` | Enable OTLP metrics export. |
| `endpoint` | string \| omitted | `null` | OTLP HTTP metrics endpoint. When omitted, uses `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`. |
| `service_name` | string | `"opensandbox-server"` | `service.name` resource attribute. |
| `export_interval_millis` | integer | `60000` | Periodic export interval (≥ 1000). |

Exported metrics:

| Metric | Type | Unit | Attributes | Description |
|-----|------|------|------------|-------------|
| `server.http.request.duration` | Histogram | `ms` | `http_method`, `http_route`, `http_status_code` | Server HTTP request latency. Histogram count provides request volume. |
| `opensandbox.sandbox.create.duration` | Histogram | `ms` | `sdk.language`, `sdk.version`, `success` | SDK-reported creation latency from create start until ready or failure. |

The HTTP metric uses the matched route template rather than the raw request path. Requests that do not reach a matched route, including early authentication failures and unmatched URLs, use `http_route=unknown`. Standard HTTP methods are recorded in uppercase, while extension methods use `http_method=OTHER` to keep attribute cardinality bounded. The metric never includes sandbox IDs, tenant IDs, API keys, request or response bodies, query strings, or other unbounded request data.

Client-side controls for the creation metric (`OPENSANDBOX_DISABLE_METRICS=1`) and pool warmup traces live in [SDK observability](/sdks/observability#creation-metrics).

## Kubernetes controller

The sandbox controller emits OTLP metrics for the pool allocation path and controller capacity. Design principles: only core signals (no speculative instrumentation), reuse what controller-runtime already exposes, and use standard OTel SDK instruments so measurements aggregate across controller replicas.

All custom controller metrics export through one process-wide global OTel meter provider, configured through the standard OpenTelemetry environment variables (see [Enabling OTLP export](#enabling-otlp-export-for-the-controller)). There are no controller-specific flags.

### Allocator-path metrics

All instruments are `Float64Histogram` on meter scope `opensandbox/controller`, recorded with unit `s` (seconds), with explicit bucket boundaries `[0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10]` (seconds). Success/error separation is expressed through the `success` boolean attribute; histogram `count` per attribute set provides the success/error counts.

| Name | Attributes | Covers | Emitted from |
|---|---|---|---|
| `controller.allocator.schedule.duration` | `namespace`, `pool_name`, `success` | The schedule decision itself (`Allocator.Schedule`, including recovery, request building, and the packing algorithm) | `pool_controller.go` `scheduleSandbox` |
| `controller.allocator.persist_alloc_state.duration` | `namespace`, `pool_name`, `success` | Persisting allocation state: the `alloc-status` annotation Patch against the API server | `allocator.go` `annoAllocationSyncer.SetAllocation` |
| `controller.allocator.sync_alloc_result.duration` | `namespace`, `pool_name`, `success` | Syncing the batch allocation result to all sandboxes of a pool (the full concurrent sync round) | `pool_controller.go` `doAllocate` |
| `controller.allocator.sync_single_alloc_result.duration` | `namespace`, `pool_name`, `success` | Per-sandbox allocation-result sync (in-memory store update + annotation Patch); recorded for both `allocated` and `released` syncs | `pool_controller.go` `syncSandboxConcurrently` |

Attribute semantics:

- `namespace`: Kubernetes namespace of the Pool (or BatchSandbox for the per-sandbox instruments).
- `pool_name`: Pool name (`pool.name` semantics; `BatchSandbox.spec.poolRef` for the per-sandbox instruments).
- `success`: `true` when the operation completed without error, `false` otherwise.

::: info Cardinality
The attribute set is intentionally bounded by (namespace, pool_name, success); there is deliberately no `sandbox_name` label. Per-sandbox identity remains available through Kubernetes objects and events; traces are the intended vehicle for per-sandbox latency attribution.
:::

### Capacity metrics

Gauge-style signals collected periodically from the informer cache, registered on the same global OTel meter provider and driven by a leader-elected manager Runnable that unregisters on shutdown:

| Name | Type | Unit | Attributes |
|---|---|---|---|
| `controller.pool.pods` | ObservableGauge | `{pod}` | `namespace`, `pool_name`, `state` (`total`/`allocated`/`available`/`updated`) |
| `controller.pool.cpu.requested` | ObservableGauge | `{cpu}` | `namespace`, `pool_name`, `state` |
| `controller.pool.memory.requested` | ObservableGauge | `By` | `namespace`, `pool_name`, `state` |
| `controller.batchsandbox.count` | ObservableGauge | `{batchsandbox}` | `namespace`, `phase`, `allocation_mode` (`pool`/`direct`) |
| `controller.batchsandbox.pods` | ObservableGauge | `{pod}` | `namespace`, `state` (`desired`/`current`/`allocated`/`ready`), `allocation_mode` |
| `controller.capacity.collect.duration` | ObservableGauge | `s` | — |

### Controller-runtime metrics (reused, not duplicated)

The controller-runtime metrics endpoint (disabled by default; enable with `--metrics-bind-address`) already exposes workqueue depth/latency, per-driver reconcile counts and durations (`controller_runtime_reconcile_*`), and REST client metrics. These are the source for reconcile health and workqueue signals; the controller does not emit competing custom versions. Pod provisioning failure recovery additionally publishes `opensandbox_pod_recovery_*` counters on that endpoint — see [pod provision failure recovery](/guides/pod-recovery).

### Enabling OTLP export for the controller

| Environment variable | Default | Description |
|---|---|---|
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | (unset) | Per-signal OTLP/HTTP endpoint, used as-is (no path appended). |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (unset) | Generic OTLP/HTTP endpoint, used as a base URL; `/v1/metrics` is appended when no path is present. Takes effect when the metrics-specific variable is unset. |
| `OTEL_EXPORTER_OTLP_METRICS_HEADERS` / `OTEL_EXPORTER_OTLP_HEADERS` | (unset) | W3C Baggage-style `key1=value1,key2=value2` headers attached to export requests. |
| `OTEL_METRIC_EXPORT_INTERVAL` | `60000` (ms) | Interval between OTLP metric exports. |
| `OTEL_SERVICE_NAME` | `opensandbox-controller` | `service.name` resource attribute. |
| `OTEL_RESOURCE_ATTRIBUTES` | (unset) | Additional resource attributes (`key1=value1,key2=value2`). |
| `OTEL_SDK_DISABLED` / `OTEL_METRICS_EXPORTER` | (unset) | Set `OTEL_SDK_DISABLED=true`, or set `OTEL_METRICS_EXPORTER` to a comma-separated list that does not include `otlp`, to disable export. |

Example (Kubernetes container env):

```yaml
env:
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: http://otel-collector.observability:4318
  - name: OTEL_METRIC_EXPORT_INTERVAL
    value: "30000"
```

Behavior:

- With no endpoint configured, the meter provider stays a no-op and nothing is exported.
- The capacity gauges register only on the elected leader (leader-elected Runnable); allocator histograms are recorded and exported on every replica.
- Export failures are retried by the OTLP exporter; setup/configuration errors never block controller startup (telemetry degrades with an error log).
- On shutdown the provider flushes pending data with a 5-second budget.

### Prometheus rendering

When exporting through an OTLP → Prometheus converter (e.g. the OpenTelemetry Collector's `prometheusremotewrite` exporter or a Prometheus OTLP receiver), dots become underscores and the unit is appended, for example:

```
controller_allocator_schedule_duration_seconds{namespace="default",pool_name="pool-a",success="true"}
```

## Execd

Meter: `opensandbox/execd` · Service name: `opensandbox-execd-<version>` (fixed by execd; `OTEL_SERVICE_NAME` is not read).

| Metric | Type | Unit | Attributes | Meaning |
|---|---|---|---|---|
| `execd.http.request.duration` | Histogram | `ms` | `http_method`, `http_route`, `http_status_code` | HTTP request latency. |
| `execd.execution.duration` | Histogram | `ms` | `operation`, `result` | Code/command execution duration. |
| `execd.filesystem.operations.duration` | Histogram | `ms` | `operation`, `result` | Filesystem operation duration. |
| `execd.system.process.count` | Observable Gauge | - | - | Current process count. |
| `execd.system.cpu.usage` | Observable Gauge | `%` | - | Sandbox CPU usage percent: sandbox cgroup when resource limits are configured, host `/proc` otherwise (e.g. microVM guests). |
| `execd.system.memory.usage_bytes` | Observable Gauge | `By` | - | Sandbox memory used bytes: sandbox cgroup when resource limits are configured, host `/proc` otherwise (e.g. microVM guests). |
| `execd.system.network.io.bytes` | Observable Counter | `By` | `direction` (`in`/`out`) | Cumulative network I/O bytes. |
| `execd.system.network.connections.active` | Observable Gauge | - | `protocol` (`tcp`/`udp`) | Current active network connections. |
| `execd.system.oom.kills` | Counter | `{kill}` | `level` (`local`/`descendant`, cgroup v2 only) | OOM kills detected in the sandbox cgroup (see below). |
| `execd.isolation.run.duration` | Histogram | `ms` | `result` | Isolated session run duration. |
| `execd.isolation.session.count` | Observable Gauge | - | - | Current number of active isolated sessions. |
| `execd.isolation.upper.usage_bytes` | Observable Gauge | `By` | - | Total bytes used by isolated session upper directories. |

When an isolated upper-directory scan fails (for example, because an active
upper directory is missing or a path cannot be read), execd omits that
collection's `execd.isolation.upper.usage_bytes` observation instead of
reporting an incorrect zero. A successfully measured empty upper still reports
zero, and the active-session count continues to be reported. Treat a missing
usage observation as **unknown**, not as zero usage.

### OOM kill observation

Execd polls its own cgroup every 5 seconds and diffs the kernel OOM kill counter: `memory.events` (`oom_kill`) on cgroup v2, `memory.oom_control` on cgroup v1. Coverage spans the whole sandbox cgroup subtree — user and business processes and nested cgroups (e.g. isolated sessions), not only the sandbox main process. cgroup v2 hosts are covered fully; cgroup v1 reports a single increment without `level`. Kernel logs (dmesg) are intentionally not used: the sandbox container has no `CAP_SYSLOG`, and the kernel ring buffer is not sandbox-scoped.

Every detection produces two signals:

- Metric `execd.system.oom.kills` with a `level` attribute: `local` for kills of processes in the sandbox cgroup itself, `descendant` for kills in nested cgroups.
- A structured warn log line on the container stdout (or the file configured with `EXECD_LOG_FILE`) with troubleshooting context:
  - `memory=<used>/<limit> peak=<peak>` — where memory stood against the cgroup limit when the detection fired
  - `events=[oom=N oom_kill=N oom_group_kill=N]` — deltas of the `memory.events` sub-counters (cgroup v2 only)
  - `kill_cgroups=a,b` — first-level child cgroups whose subtree received the kills
  - `exited=[pid=N comm=x ppid=N rss=N age=Ns, ...]` — processes that disappeared from `/proc` between the polls surrounding the event (candidates for victims; bounded by the kill count, capped at 5 entries)

On warm sandboxes rebound by `POST /internal/init`, the counter interval that crosses a binding handoff is dropped rather than attributed to the new sandbox identity.

### Shared attributes

- `sandbox_id` from `OPENSANDBOX_ID` (when set)
- extra key/value attributes from `OPENSANDBOX_EXECD_METRICS_EXTRA_ATTRS` (when set)

Execd additionally serves local CPU/memory snapshots and live streams through its `/metrics` and `/metrics/watch` APIs, independent of OTLP export — see [execd architecture](/architecture/data-plane/execd#observability).

### Endpoint configuration

Export is enabled only when at least one OTLP endpoint is set (`OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`, fallback `OTEL_EXPORTER_OTLP_ENDPOINT`); otherwise execd keeps metrics local. If both are unset, the node-IP fallback described [above](#export-endpoints) applies.

```bash
export OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://otel-collector:4318"
```

## Egress sidecar

Meter: `opensandbox/egress` · Service name: `opensandbox-egress-<version>`.

| Metric | Type | Unit | Meaning |
|---|---|---|---|
| `egress.dns.query.duration` | Histogram | `s` | Upstream DNS forward latency (recorded for allowed queries). |
| `egress.dns.query.failed_total` | Counter | - | Queries the proxy could not resolve, by `reason`. |
| `egress.dns.reply.failed_total` | Counter | - | Reply writes that failed after a decision, by `stage`. A nonzero count means a query was handled but its answer never reached the client. |
| `egress.policy.denied_total` | Counter | - | Number of DNS queries denied by policy. |
| `egress.nftables.rules.count` | Observable Gauge | `{element}` | Approximate policy size after last successful static apply (fast-sandbox profile: summed across every installed subject's policy, 0 while deny-first). |
| `egress.nftables.updates.count` | Counter | - | Number of successful nftables updates (static apply + dynamic IP add). |
| `egress.nftables.updates.failed_total` | Counter | - | nftables updates that failed, by `operation`. |
| `egress.system.memory.usage_bytes` | Observable Gauge | `By` | **Node** memory used bytes (Linux: gopsutil; non-Linux build: `0`). |
| `egress.system.cpu.utilization` | Observable Gauge | `1` | **Node** CPU busy ratio in `[0,1]` (Linux: gopsutil; non-Linux build: `0`). |
| `egress.process.memory.usage_bytes` | Observable Gauge | `By` | Memory charged to the sidecar's own cgroup. Only present when cgroupfs is readable. |
| `egress.process.cpu.time` | Observable Counter | `s` | CPU seconds consumed by the sidecar's own cgroup. Only present when cgroupfs is readable. |

### `system` vs `process`

They measure different things, and the difference matters because this sidecar runs **per sandbox**:

- `egress.system.*` comes from gopsutil, i.e. `/proc/meminfo` and `/proc/stat`, which inside a container describe the **node**. Every sandbox on a node therefore publishes the same figure under its own `sandbox_id`. Do not chart these "by sandbox": the series look per-sandbox but are N copies of one node number. Prefer kubelet/cAdvisor or a node exporter for node-level data.
- `egress.process.*` is read from the sidecar's own cgroup (v2 `memory.current` and `cpu.stat`, falling back to v1 `memory.usage_in_bytes` and `cpuacct.usage`), so it really is per sandbox.

`egress.process.cpu.time` is a **cumulative counter of consumed seconds**, not a sampled ratio: use `rate()` on it. Both `process` instruments are registered only if their cgroup files can be read — a runtime that does not expose cgroupfs (a sandbox pod under `secure_runtime`, for instance) gets no series at all, rather than a flat zero that reads like an idle sidecar.

### DNS latency histogram boundaries

`egress.dns.query.duration` declares its bucket boundaries explicitly:

```
0.001  0.0025  0.005  0.01  0.025  0.05  0.1  0.25  0.5  1  2.5  5  10  15  30  60  120  300  600
```

The instrument records **seconds**, while the SDK default boundaries are the spec's millisecond ladder (`0, 5, 10, … 10000`), so every realistic latency would fall into the single `le=5` bucket and the quantiles would be meaningless. The coarse tail exists because the recorded duration covers the **whole resolver chain**: forwarding walks the upstreams serially, each with the full timeout (`OPENSANDBOX_EGRESS_DNS_UPSTREAM_TIMEOUT`, 2s by default), so a query can legitimately take `timeout x len(upstreams)`. A late **success** lands in the tail too, not only an exhausted failure. The chain has no finite worst case (`OPENSANDBOX_EGRESS_DNS_UPSTREAM` accepts an unbounded resolver list), so past the last boundary quantile resolution is lost by construction and `_count` is what remains. Both successful and failed lookups feed this histogram, so its tail mixes slow resolutions with exhausted retry chains.

### Failure signals

`egress.dns.query.failed_total` and `egress.policy.denied_total` answer different questions, and confusing them inverts the diagnosis:

- **denied** — the policy did its job. The workload asked for something it is not allowed to reach. Expected traffic in a working system.
- **failed** — the sidecar could not do its job. The workload asked for something allowed and got `SERVFAIL`. Never expected.

`reason` comes from a closed set, so the counter's cardinality is fixed and neither the queried name nor the error text is ever attached:

| `reason` | Meaning |
|---|---|
| `no_upstreams` | No resolvers configured or discovered. |
| `upstream_error` | Every resolver failed to answer (network error, timeout). |
| `empty_response` | A resolver returned a nil message. |
| `rcode` | The last resolver answered with a failover-worthy rcode, e.g. `SERVFAIL`. |

`egress.nftables.updates.failed_total` covers another silent failure class. Its `operation` attribute is one of `static_apply`, `dynamic_add`, `remove`, or — in the fast-sandbox profile (OSEP-0022) — `deny_first`, `reset`. `dynamic_add` is the one to alert on: a failed add means the kernel never learned about IPs the policy allows, so the chain drops traffic that should pass — which looks exactly like a policy denial from inside the sandbox while `egress.policy.denied_total` stays flat.

`egress.dns.reply.failed_total` covers the last silent failure class: a query that was **handled** (decided, maybe forwarded and answered upstream) whose reply write then failed. Its `stage` attribute is one of `malformed`, `unknown_source`, `deny`, `upstream_error`, `answer`, and every failure also emits a `[dns] reply write failed (stage=… remote=… question=…)` warning with the remote address and query name. Alert on any nonzero value: like `dynamic_add`, an `answer`-stage failure means traffic the policy allows is not reaching the client.

A `static_apply` failure happens during startup, where the sidecar logs and exits. Metrics leave through a periodic reader and `os.Exit` skips the deferred shutdown, so that path flushes telemetry explicitly before terminating — otherwise the one sample explaining why the sidecar died would never be exported.

### Automatic egress allow rule

When an OTLP destination is configured — the endpoint env vars, or the exporter fallback node IP (`HOST_IP` / `/etc/hostinfo`) when both are unset — egress automatically injects an always-allow egress rule for that host (domain or IP, any port), so telemetry export works under the default deny-all policy without manually managing allowlist rules. This also covers the sidecar's own metric export, which shares the sandbox network namespace and would otherwise be blocked by its own egress chain.

- Precedence: `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` wins over `OTEL_EXPORTER_OTLP_ENDPOINT`; the fallback node IP applies only when neither is set. A set-but-invalid endpoint never falls back (the exporter does not either), so no rule is injected in that case.
- The endpoint must be a URL (`https://host:4318/v1/metrics`) — the `otlpmetrichttp` env-var form. Bare `host:port` or `host` values are not accepted; a trailing root dot on FQDNs is trimmed to match DNS policy normalization.
- The rule lives in the always-allow layer: it survives user `POST`/`PATCH`/`DELETE` policy updates and always-rule file reloads. Operators can still block the target with `deny.always`, which takes precedence.
- Rules are host-scoped (any port), matching the egress rule model; ports are not enforced per rule.

::: warning
Use a fully-qualified service name or an IP in the endpoint. Single-label names (e.g. `otel-collector`) are subject to resolver search-domain expansion, and the deny-all DNS proxy answers the expanded names (e.g. `otel-collector.<ns>.svc.cluster.local`) with NXDOMAIN without falling back to the bare name, so the auto-generated exact-host allow rule would not be reached.
:::

### Shared attributes

- `sandbox_id` from `OPENSANDBOX_EGRESS_SANDBOX_ID` (when set). Without it the sidecars of different sandboxes export identical attribute sets, so their series collide in the backend — which matters most for the per-sandbox `egress.process.*` gauges.
- extra key/value attributes from `OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS` (when set)

### Endpoint configuration

Export is enabled only when at least one OTLP endpoint is set; otherwise egress keeps metrics local (no OTLP export), subject to the node-IP fallback described [above](#export-endpoints).

```bash
export OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://otel-collector.sandbox.svc.cluster.local:4318"
# An IP endpoint works as well:
export OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://10.0.0.5:4318"
```

The server can inject the endpoint into every egress sidecar via `[egress].otlp_endpoint`; see [egress observability](/architecture/network/egress#observability-opentelemetry).

### Structured logs (JSON)

Egress structured logs are emitted by zap (typically to stdout). OTLP log export is not implemented in-tree.

- Common fields: `sandbox_id` (when `OPENSANDBOX_EGRESS_SANDBOX_ID` is set), key/value pairs from `OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS` merged into the root logger, and `opensandbox.event` identifying the event family.
- `opensandbox.event=egress.outbound` — emitted on allow-path DNS handling (success or forward error) with `target.host`, `target.ips`, `peer`, and `error`.
- Policy lifecycle: `opensandbox.event=egress.loaded` (initial effective policy loaded), `egress.updated` (policy update applied), `egress.update_failed` (policy update failed), with `egress.default` (`allow`/`deny`), `rules`, and `error` fields.

## Ingress gateway

Meter: `opensandbox/ingress` · Service name: `opensandbox-ingress-<version>`.

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

### Wake-on-access (OSEP-0024)

Emitted when wake-on-access is enabled. Restore saturation, parking-lot shedding, policy gate failures, and Redis write drops are the signals to watch.

| Metric | Type | Unit | Attributes | Description |
|---|---|---|---|---|
| `ingress.wake.flights` | Counter | — | `outcome` (`triggered`/`joined`/`none`) | Wake flight count by outcome |
| `ingress.park.active` | UpDownCounter | — | - | Currently parked requests |
| `ingress.park.wait.duration` | Histogram | `s` | `outcome` (`served`/`budget_exhausted`/`canceled`/`error`) | Per-parked-request wait duration; buckets are sized around the default 5s park budget (a ~200ms same-node resume vs a near-exhaustion wait) |
| `ingress.park.shed` | Counter | — | - | Requests shed because the parking lot was full |
| `ingress.wake.restore.active` | UpDownCounter | — | - | Wake flights currently holding a restore permit (the concurrent-restore gauge) |
| `ingress.idlep.policy.reads` | Counter | — | `outcome` (`allowed`/`denied`/`error`) | Idle-policy gate outcomes; sustained `error` means an RBAC or apiserver problem |
| `ingress.activity.writes_dropped` | Counter | — | - | Activity observations dropped before reaching Redis |

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

### Endpoint configuration

```bash
export OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://otel-collector:4318"
```

Fallback: `OTEL_EXPORTER_OTLP_ENDPOINT`. When both are unset, export falls back to the node IP (`HOST_IP` env, then `/etc/hostinfo`) on port 4318 over plain HTTP; if no node IP resolves, metrics are not exported. Disable export with `OTEL_SDK_DISABLED=true` or `OTEL_METRICS_EXPORTER=none`. Default temporality: delta for synchronous Counter/Histogram, cumulative for observable gauges — override with `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE`.

## Node agent

The node agent's log pipeline exports OTLP metrics with no sandbox or Pod identifiers as labels:

| Metric | Type | Unit | Description |
|---|---|---|---|
| `opensandbox.nodeagent.records` | Counter | - | Records consumed from sources and handed to the sink. |
| `opensandbox.nodeagent.bytes` | Counter | `By` | Record payload bytes consumed. |
| `opensandbox.nodeagent.drops` | Counter | - | Records dropped (only possible under `drop` backpressure). |
| `opensandbox.nodeagent.retries` | Counter | - | Sink delivery retry attempts. |
| `opensandbox.nodeagent.queue.bytes` | UpDownCounter | `By` | Bytes currently queued for delivery (queue depth proxy). |
| `opensandbox.nodeagent.sink.consume.duration` | Histogram | `ms` | Sink consumption (write) latency. |

It requires an explicit endpoint — the node-IP fallback used by other components is disabled (`DisableEndpointFallback`):

```yaml
opensandbox-node-agent:
  extraEnv:
    - name: OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
      value: http://otel-collector.observability:4318/v1/metrics
```

See [node agent configuration](/guides/node-agent) for the full agent setup.

## SDK metrics

The SDKs report sandbox creation latency to the lifecycle server (`POST /v1/metrics/events`), which the server exports as `opensandbox.sandbox.create.duration` when `[otel]` is enabled, and can emit pool warmup traces through the application's own OpenTelemetry provider. Client-side enablement and disablement are covered in [SDK observability](/sdks/observability).
