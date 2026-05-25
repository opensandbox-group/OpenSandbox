---
title: OpenTelemetry Metrics and Logs (execd, egress, and ingress)
authors:
  - "@Pangjiping"
  - "@ninan-nn"
creation-date: 2026-03-18
last-updated: 2026-04-12
status: implementing
---

# OSEP-0010: OpenTelemetry Metrics and Logs (execd, egress, and ingress)

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [1. Metrics](#1-metrics)
    - [1.1 execd metrics](#11-execd-metrics)
    - [1.2 egress metrics](#12-egress-metrics)
    - [1.3 ingress metrics](#13-ingress-metrics)
  - [2. Logging](#2-logging)
    - [2.1 Egress: sandbox outbound access log (hostname / IP)](#21-egress-sandbox-outbound-access-log-hostname--ip)
    - [2.2 Egress: policy initialization and allow/deny rule changes](#22-egress-policy-initialization-and-allowdeny-rule-changes)
  - [3. Initialization and configuration](#3-initialization-and-configuration)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

This proposal introduces unified **OpenTelemetry** instrumentation for OpenSandbox’s three Go components—**execd**, **egress**, and **ingress**—covering **Metrics** and **Logs** only (**no OpenTelemetry Tracing / distributed traces**). With OTLP export and environment-based configuration, operators and developers can observe request rates, latencies, resource usage, policy enforcement, **egress outbound hostname/IP access (via structured logs, not metrics)**, **egress policy load and allow/deny rule changes (structured logs)**, and ingress proxy traffic in production and integrate with existing observability stacks (e.g., Prometheus, Grafana Loki).

**Egress (reference implementation):** OTLP **metrics** only (`components/internal/telemetry` + `components/egress/pkg/telemetry`); structured logs are **zap JSON to stdout** (and the same **metric dimensions** are merged onto the root logger—`sandbox_id` + `OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS`). **OTLP log export is not implemented** for egress in-tree; execd/ingress may still follow the broader “metrics + logs” story below where implemented.

## Motivation

Today execd, egress, and ingress have partial observability (e.g., execd’s HTTP API and `GetMetrics`/`WatchMetrics`, zap/loggers in egress and ingress) but lack:

- **Standardized metrics**: No Prometheus/OpenTelemetry-style HTTP QPS, latency, status codes; no unified metrics for execd code execution and Jupyter sessions, egress DNS/policy, or ingress proxy requests and routing.
- **Unified export**: No OTLP endpoint or configuration for metrics and logs, so integration with a central observability platform is difficult.

Adopting OpenTelemetry for **metrics and logs** allows the three components to gain consistent signals without changing core business logic, with the ability to disable export via environment variables for deployments without observability backends.

### Goals

- Integrate the OpenTelemetry SDK (Go) into execd, egress, and ingress to emit **Metrics** and **Logs** via OTLP (and structured stdout logs where applicable).
- **Metrics**: Cover HTTP, code execution, Jupyter, filesystem operations, and system resources (execd); DNS, policy, nftables, and system resources (egress); HTTP/WebSocket proxy requests, routing resolution, status codes, and system resources (ingress).
- **Logging**: Use structured fields on the existing zap logger; for **egress**, **by default** emit **structured log records** for each outbound DNS attempt (hostname and resolved IPs when available), and for **initial egress policy load** and **runtime allow/deny rule updates** (POST/PATCH to the policy API or equivalent), all at **info** unless the deployment lowers the logger level.
- **Configuration**: Provide initialization and support for OTLP exporters and environment variables; default to no export so deployments without observability backends are unaffected.

### Non-Goals

- **OpenTelemetry Tracing** (distributed traces, `TracerProvider`, Jaeger-style request graphs) for execd, egress, or ingress — **out of scope** for this OSEP; correlation relies on **metrics dimensions**, **structured log fields** (e.g. `sandbox_id` on egress), and time—not on `trace_id`/`span_id`.
- Do not replace existing execd HTTP metric endpoints such as `GetMetrics`/`WatchMetrics`; they can coexist with OpenTelemetry metrics.
- Do not implement OpenTelemetry on the server (Python) in this proposal; scope is limited to the three Go components (execd, egress, ingress).
- Do not commit to vendor-specific backends (e.g., Datadog, New Relic); export is via the standard OTLP protocol only.
- Do not require a Collector; both direct OTLP and via-Collector export are supported.

## Requirements

| ID | Requirement | Priority    |
|----|-------------|-------------|
| R1 | execd/egress/ingress support exporting **Metrics** and **Logs** via OTLP (**not** Traces) | Must Have   |
| R2 | Metrics cover all execd, egress, and ingress metric items listed in this proposal | Must Have   |
| R3 | Structured logs include a filterable **sandbox identifier** where applicable (`sandbox_id` or equivalent) | Must Have   |
| R4 | Configuration via environment variables (endpoint, toggles) without code changes | Must Have   |
| R5 | Default or unset OTLP config results in no export to avoid impacting deployments without observability | Must Have   |
| R6 | Compatible with existing zap Logger interface; no breaking changes to Logger abstraction | Should Have |
| R7 | Egress can emit structured logs for per–sandbox outbound access (DNS question hostname and, when present, resolved IPs); **not** required as Prometheus-style metrics with hostname/IP as labels | Should Have |
| R8 | Egress emits structured logs when **initial policy** is applied and when **policy rules are added or changed** at runtime; logs include `sandbox_id` and a **summary** of the effective rules (see [§2.2](#22-egress-policy-initialization-and-allowdeny-rule-changes)) | Should Have |

## Proposal

Introduce an **OpenTelemetry initialization module** in the main startup of execd, egress, and ingress that:

1. Creates and registers a **MeterProvider** and **MetricReader** (e.g., OTLP metric exporter).
2. Optionally creates a **LoggerProvider** and registers an OTLP log exporter; otherwise rely on zap JSON logs and optional **Logs Bridge** to OTLP (**egress:** metrics-only OTLP; logs stay on stdout unless collected by an agent).
3. Reads OTLP endpoint, service name, etc., from environment variables (or config files).

Application code records metrics on critical paths. **HTTP metrics** for execd/egress/ingress use **aggregated dimensions** (route template, status code, method; **execd current keys:** `http_route`, `http_status_code`, `http_method`)—**without** OpenTelemetry spans: implement via **manual instrumentation** or thin middleware that only increments histograms/counters (no `TracerProvider`). Egress and ingress use `net/http`; execd uses Gin—same principle (metrics only, no trace spans).

### Notes/Constraints/Caveats

- OpenTelemetry Go SDK version and stability must match the project’s Go version; prefer the stable API (e.g., `go.opentelemetry.io/otel` v1).
- Metric names and attributes should follow [OpenTelemetry semantic conventions](https://opentelemetry.io/docs/specs/semconv/) where practical for compatibility with generic dashboards.
- egress may run as a sidecar in the same Pod as the workload; keep metric export batching configurable to limit sidecar CPU/memory.
- Log enhancements apply only to code paths using the shared Logger; code that uses the standard `log` package is out of scope for this proposal but can be migrated later.

### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| OTLP export failures or unreachable endpoint cause blocking or retry storms | Use async export, configurable timeouts and queue limits; on failure only log locally and do not affect the main flow |
| High metric cardinality (e.g., per sandbox_id or raw URL path) | Avoid high-cardinality dimensions: only use aggregated dimensions such as status_code, operation; **HTTP metrics must use the route template dimension** (execd current key: `http_route`, e.g. `/code/contexts/:contextId`), not the raw request path, or execd routes with path parameters will produce high-cardinality series that are hard to operate |
| Divergence from existing metrics APIs | Leave existing HTTP metric endpoints unchanged; OpenTelemetry metrics are additive |

## Design Details

### 1. Metrics

#### 1.1 execd metrics

| Category | Metric name (suggested) | Type | Description |
|----------|-------------------------|------|-------------|
| **HTTP** | `execd.http.request.duration` | Histogram | Request latency (ms) by `http_method`, **`http_route` (route template)**, `http_status_code` |
| **Code execution** | `execd.execution.duration` | Histogram | Duration per execution with attributes `operation` (e.g. `run_code`/`run_in_session`/`run_command`) and `result` (derived from execution callbacks) |
| **Filesystem** | `execd.filesystem.operations.duration` | Histogram | Operation duration with attributes `operation` (upload/download/search/replace/chmod/rename/mkdir/rmdir/delete/info) and `result` (`success`/`failure`) |
| **System** | `execd.system.cpu.usage` | Gauge | System CPU usage percent (from gopsutil) |
| | `execd.system.memory.usage_bytes` | Gauge | Memory usage |
| | `execd.system.process.count` | Gauge | Current number of processes in the system |

All metrics are created via the OpenTelemetry Meter; metric names/units/attributes are implementation-defined and keep low-cardinality dimensions.

**Execd HTTP dimensions:** Several execd routes embed identifiers in the URL (e.g. `/code/contexts/:contextId`, `/session/:sessionId/run`, `/command/status/:id` in `components/execd/pkg/web/router.go`). Using the raw request path as a metric dimension would create high-cardinality time series and make OTLP/Prometheus metrics hard to operate. Therefore **the route template must be used as the dimension**: `http_route` (e.g. `/code/contexts/:contextId`), not the actual request path (e.g. `/code/contexts/abc-123`). Record the matched route pattern from Gin (e.g. `c.FullPath()` or equivalent; fallback `unknown`) in metric attributes—**without** OpenTelemetry tracing middleware.

#### 1.2 egress metrics

| Category | Metric name (implemented) | Type | Description |
|----------|---------------------------|------|-------------|
| **DNS** | `egress.dns.query.duration` | Histogram (`s`) | Upstream forward latency after policy **allow** (success and forward error); **deny** does not record |
| **Policy** | `egress.policy.denied_total` | Counter | +1 per DNS query **denied** by policy |
| **nftables** | `egress.nftables.rules.count` | Observable Gauge (`{element}`) | Approximate static policy size after last successful **`ApplyStatic`** (`dns+nft`); stays **0** in **dns-only** or before any apply |
| **nftables** | `egress.nftables.updates.count` | Counter | +1 per successful **`ApplyStatic`** (incl. retry path) or **`AddResolvedIPs`** |
| **System** | `egress.system.memory.usage_bytes` | Observable Gauge (`By`) | Host RAM in use (Linux: `/proc/meminfo`; non-Linux: **0**) |
| **System** | `egress.system.cpu.utilization` | Observable Gauge (`1`) | CPU busy ratio **0–1** between scrapes (Linux: `/proc/stat`; first scrape **0**; non-Linux: **0**) |

Meter name: **`opensandbox/egress`**.

**Per–sandbox outbound hostname / IP (monitoring vs logs):** Operators often want a record of **which hostnames or IPs** a sandbox attempted to reach. **Do not** encode raw hostname or per-IP destination as **metric labels** or default metric dimensions: that creates extreme cardinality in Prometheus-style backends and conflicts with the cardinality controls elsewhere in this OSEP. Instead, treat **each DNS egress attempt** (and its outcome) as a **structured log event** (see [§2.1](#21-egress-sandbox-outbound-access-log-hostname--ip)). Aggregated **metrics** above remain suitable for rates, latency, and allow/deny counts without naming every destination.

**Resource vs metric attributes (egress reference implementation):**

- **Resource** (OTLP): `service.name` = **`opensandbox-egress-<version>`** (build/version from `internal/version`), and optional **`sandbox_id`** when **`OPENSANDBOX_EGRESS_SANDBOX_ID`** is set (`components/egress/pkg/telemetry/init.go`).
- **Metric datapoint attributes** (every recorded measurement): optional **`sandbox_id`** (same env var value) and any extra **`key=value`** segments from **`OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS`** (comma-separated; first `=` splits key/value per segment). Low-cardinality only. The **same** attributes are also attached to the **zap** root logger so log lines align with metric series.
- Structured log **event** fields for sandbox identity use the same key **`sandbox_id`** as Resource and metrics (see [§2](#2-logging)) so cross-signal filtering and dashboards use one dimension name.

#### 1.3 ingress metrics

| Category | Metric name (suggested) | Type | Description |
|----------|-------------------------|------|-------------|
| **HTTP** | `ingress.http.request.count` | Counter | Request count by method, status_code, proxy_type (http/websocket) (QPS derivable) |
| | `ingress.http.request.duration` | Histogram | Request duration (including routing and proxy) by method, proxy_type |
| **Routing** | `ingress.routing.resolutions.count` | Counter | Resolutions by result (success/not_found/not_ready/error) |
| | `ingress.routing.resolution.duration` | Histogram | Time to resolve sandbox target (from cache or API) |
| **Proxy type** | `ingress.proxy.http.requests_total` | Counter | HTTP proxy request count |
| | `ingress.proxy.websocket.connections_total` | Counter | WebSocket connection count |
| **System** | `ingress.system.cpu.usage` | Gauge | CPU usage |
| | `ingress.system.memory.usage_bytes` | Gauge | Memory usage |

Note: Ingress typically returns 200 (success), 400 (bad request), 404 (sandbox not found), 502 (upstream error), 503 (sandbox not ready); aggregate by `http.status_code` for error-rate monitoring.

Metric namespaces are `execd.*`, `egress.*`, and `ingress.*` for easy filtering in a shared backend. **Execd (current implementation)** attaches `sandbox_id` from `OPENSANDBOX_ID` when set; ingress obtains sandbox-related identifiers from routing context where applicable.

### 2. Logging

**Egress structured log keys:** event type is **`opensandbox.event`**; policy defaults use **`egress.default`**; **policy rule summaries** use the top-level key **`rules`** (array of `{action,target}`). Shared across [§2.1](#21-egress-sandbox-outbound-access-log-hostname--ip) and [§2.2](#22-egress-policy-initialization-and-allowdeny-rule-changes); each rule entry uses **`action`** and **`target`** per API shape.

- **Structured fields**: Use existing `Logger.With(Field{...})` for stable keys (`sandbox_id` on egress for sandbox identity; component-specific attributes elsewhere as needed).
- **Context-aware**: Handlers that receive `context.Context` may attach request-scoped fields to logs where useful; **no** requirement for `trace_id`/`span_id` from OpenTelemetry (tracing out of scope).
- **Filter/query by sandbox id**: When a request or operation is associated with a sandbox (e.g. execd handling a request for that sandbox, ingress proxying to that sandbox), log records **should** include a filterable sandbox identifier (**`sandbox_id`** on egress; **`osbx.id`** or equivalent where other components standardize it) so that log backends can filter and query by sandbox for per-sandbox debugging.
- **OTLP Logs:** Where a component implements OTLP log export, records carry the same structured fields. **Egress (reference):** no OTLP log pipeline—stdout JSON only; use Collector/file agents if centralization is required.
- **Default logger fields (egress):** In addition to per-event `With` fields, the root logger includes **`sandbox_id`** and **`OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS`** key/values when set (aligned with metric dimensions).

The existing `Logger` interface (`Infof`, `With`, `Named`) stays unchanged.

**Egress log families (summary):** All egress structured logs use **zap** `With` fields plus a human-readable **`msg`**. **`sandbox_id`** comes from **`OPENSANDBOX_EGRESS_SANDBOX_ID`** when set. Two event families:

| Family | `opensandbox.event` | Typical level | One line per |
|--------|---------------------|---------------|--------------|
| **Outbound access** | **`egress.outbound`** | `info` (default) | Each observed outbound attempt (DNS path and/or IP-only path per [§2.1](#21-egress-sandbox-outbound-access-log-hostname--ip)) |
| **Policy lifecycle** | **`egress.loaded`**, **`egress.updated`**, **`egress.update_failed`** | `info` / `warn` / `error` | Policy load, successful update, or failed update ([§2.2](#22-egress-policy-initialization-and-allowdeny-rule-changes)) |

#### 2.1 Egress: sandbox outbound access log (hostname / IP)

**Purpose:** Support **monitoring and audit** use cases that need “what did this sandbox try to reach?” with **hostname** (DNS question name) and, when applicable, **resolved IP addresses**—without turning every destination into a metric time series.

**Signal type:** **Logs** (structured fields on the existing zap logger; **egress** ships **stdout** JSON only in-tree). Identify records with **`opensandbox.event` = `egress.outbound`**; this is **not** a per-destination **Counter/Histogram metric** (per-destination “log metrics” in vendor UIs remain **log-derived**, e.g. Loki queries, not OpenTelemetry Metrics with hostname labels).

**Observation points:** (1) **DNS proxy** — each client DNS query is one “access attempt” to a name (after policy evaluation; after upstream resolution or on forward error). (2) **IP-only path** (e.g. direct connect to a literal IP, or enforcement observed without a DNS name) — log using **`peer`** instead of DNS-specific fields.

**Recommended fields** for **`opensandbox.event` = `egress.outbound`** (align with OpenTelemetry semantic conventions where possible):

| Field | Description |
|-------|-------------|
| `opensandbox.event` | Constant **`egress.outbound`** |
| `sandbox_id` | Sandbox identifier from **`OPENSANDBOX_EGRESS_SANDBOX_ID`** when set |
| `target.host` | Normalized QNAME when the attempt is **name-based** (lowercase, trailing dot stripped). **Omit** when the attempt is **IP-only** (use `peer` instead). |
| `target.ips` | When the DNS response includes A/AAAA: list of resolved IPs as strings. **Omit or empty** when not applicable (non-address RR types, **IP-only** path, or forward **error** before resolution). |
| `peer` | Destination **IP address** when the attempt is **IP-only** (no `target.host`), or when logging the peer IP is required without a DNS query name. **Omit** when only DNS name + `target.ips` describe the destination. |
| `error` | On upstream DNS **forward failure**: short message (e.g. timeout). **Omit** on success. |

Policy **deny** is not represented in this log (use **`egress.policy.denied_total`** and optional webhooks); implementations may omit a dedicated `result` field.

**Examples (logical JSON shape; zap may add `level`, `ts`, `logger`, `msg`):**

```json
{"opensandbox.event":"egress.outbound","sandbox_id":"sb-abc","target.host":"pypi.org","target.ips":["151.101.0.223"]}
```

```json
{"opensandbox.event":"egress.outbound","sandbox_id":"sb-abc","target.host":"example.com","error":"i/o timeout"}
```

```json
{"opensandbox.event":"egress.outbound","sandbox_id":"sb-abc","peer":"198.51.100.7"}
```

**Trace correlation:** **Not applicable** under this OSEP (no distributed traces). **Correlation** uses `sandbox_id`, destination fields (`target.host` and/or `target.ips` and/or `peer`), and timestamps.

**Default behavior:** Emit one **info**-level structured record per handled DNS query on this path (**default on**). Under high DNS QPS this can produce **large log volume**; document clearly. Operators who need less verbosity may lower the component log level (e.g. `warn`) via existing egress log configuration—**no** separate env toggle for these events in this OSEP. This is separate from `OTEL_LOGS_EXPORTER`: local structured JSON logs may suffice without OTLP log export.

**Privacy / retention:** Document that full outbound access logs may be sensitive; operators should set retention and access controls in their log backend.

#### 2.2 Egress: policy initialization and allow/deny rule changes

**Purpose:** Provide an **audit trail** for **which egress policy** a sandbox sidecar is enforcing: first load at process start and every **successful** update when operators or the control plane add or change **allow/deny** rules. This is **low frequency** compared to per-DNS [§2.1](#21-egress-sandbox-outbound-access-log-hostname--ip) and uses default **info**-level structured logs (**default on**).

**Signal type:** **Logs** (structured fields; egress: stdout JSON in-tree). **Not** metrics with one time series per rule target (same cardinality rationale as §1.2).

**When to emit**

| Event | `opensandbox.event` (suggested) | When |
|-------|----------------------------------|------|
| **Initial policy load** | `egress.loaded` | Once the effective **initial** policy is known after startup: e.g. from **`OPENSANDBOX_EGRESS_POLICY_FILE`** (if valid), else **`OPENSANDBOX_EGRESS_RULES`**, else built-in default (e.g. deny-all). If the lifecycle server later **POSTs** policy after `/healthz`, emit **`egress.loaded`** when that first server-driven snapshot is applied, or **`egress.updated`** if implementation treats it strictly as an update—choose one convention and document it so operators do not see duplicate semantics. |
| **Runtime rule change** | `egress.updated` | After a **successful** application of new policy via the egress HTTP API (e.g. POST/PATCH **`/policy`** with auth), including additions or modifications of allow/deny rules. On **failure** (4xx/5xx, validation error), log at **warn** or **error** with `opensandbox.event` e.g. `egress.update_failed` and **no** effective policy change. |

**Recommended fields (common)**

| Field | Description |
|-------|-------------|
| `opensandbox.event` | `egress.loaded` \| `egress.updated` \| `egress.update_failed` |
| `sandbox_id` | From `OPENSANDBOX_EGRESS_SANDBOX_ID` when set |
| `egress.default` | Effective `defaultAction` after apply (e.g. `allow` / `deny`) |
| `rules` | **`egress.loaded`**: full effective `egress` list. **`egress.updated`**: **this request only** — **PATCH**: patch body rules; **POST/PUT**: body `egress` (replacement); **reset**: `[]`. Omit on **`egress.update_failed`** |
| `error` | On **`egress.update_failed`**: validation or transport message |

**Recommended fields (rule summary — not raw body)**

- Include a compact representation of **allow/deny rules** sufficient for audit: e.g. **`rules`** as an array of objects `{ "action": "allow|deny", "target": "<string>" }` in **stable order** (e.g. as stored after parse), **or** a **digest** (hash) of the canonical JSON if full rule listing is too large for log pipelines. For **`egress.updated`**, **`rules`** should reflect **only the current API request** (incremental PATCH or POST body egress), not the full merged policy after apply.
- Avoid logging **secrets** (policy JSON may embed sensitive hostnames in some deployments); document that **targets** are part of policy and may need redaction in highly regulated environments.

**Examples (logical JSON shape):**

```json
{"opensandbox.event":"egress.loaded","sandbox_id":"sb-abc","egress.default":"deny","rules":[{"action":"allow","target":"pypi.org"},{"action":"allow","target":"*.github.com"}]}
```

```json
{"opensandbox.event":"egress.updated","sandbox_id":"sb-abc","egress.default":"deny","rules":[{"action":"allow","target":"api.openai.com"}]}
```

(Example shows a single-rule **PATCH**; **`egress.default`** is the effective policy after apply.)

```json
{"opensandbox.event":"egress.update_failed","sandbox_id":"sb-abc","error":"validation failed: rule limit exceeded"}
```

**Trace correlation:** **Not applicable** under this OSEP. For **`egress.updated`**, correlation with the HTTP request is via **HTTP access logs** (e.g. method, path, status) and **time**; for **`egress.loaded`** at startup, resource attributes (`service.name`, pod name) identify the instance.

**Volume:** Policy events are **infrequent** compared to [§2.1](#21-egress-sandbox-outbound-access-log-hostname--ip). Emit **`egress.loaded`** and **`egress.updated`** at **info** when policy changes (**default on**). **No** separate env toggle in this OSEP; deployments that must not log policy content should use log level or pipeline-side filtering.

### 3. Initialization and configuration

- **Initialization**  
  Implement `InitOpenTelemetry(ctx context.Context, opts InitOptions) (shutdown func(), err error)` in main for execd, egress, and ingress (or in a shared `pkg/telemetry`):
  - Create `MeterProvider` and register an OTLP metric exporter (e.g., `go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp` or gRPC).
  - Optionally create `LoggerProvider` and register an OTLP log exporter; otherwise rely on zap and optional **Logs Bridge** (execd/ingress as applicable).
  - **Egress (reference):** `components/internal/telemetry` initializes **MeterProvider + OTLP HTTP metrics** only; **no** `LoggerProvider`; zap remains the sole log sink.
  - Set global `otel.SetMeterProvider` (and logger provider if used), and return a `shutdown` function (Flush + ForceFlush) to call on process exit.
  - **Do not** set `TracerProvider` for production use; omit or use a no-op tracer provider.

- **OTLP exporter**  
  Support HTTP and gRPC OTLP endpoints via environment variables:
  - `OTEL_EXPORTER_OTLP_ENDPOINT` (or per-signal `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`, `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`).
  - If unset, do not export or use a Noop provider to avoid connection errors.

- **Environment variables**  
  Support at least (names follow OpenTelemetry conventions where applicable):
  - `OTEL_SERVICE_NAME`: service name (execd / egress / ingress).
  - `OTEL_EXPORTER_OTLP_ENDPOINT` (or per-signal endpoints for metrics/logs).
  - `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER` (e.g., `none` to disable).
  - `OTEL_RESOURCE_ATTRIBUTES`: key-value pairs for resource attributes (e.g., deployment.env);
  - **`OPENSANDBOX_ID`** (execd): when set, `sandbox_id` is attached to the OTLP Resource and to every execd metric datapoint.
  - **`OPENSANDBOX_EXECD_METRICS_EXTRA_ATTRS`** (execd): optional comma-separated `key=value` pairs appended to every execd metric datapoint.
  - **`OPENSANDBOX_EGRESS_SANDBOX_ID`** (egress): sets **`sandbox_id`** on the OTLP **Resource**, on **every metric measurement**, and in structured log events (same key everywhere). When unset, omitted from all of the above.
  - **`OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS`** (egress): optional comma-separated **`key=value`** pairs appended to **every metric measurement** and to the **root zap logger** (see [§1.2](#12-egress-metrics)).

Egress structured logs for outbound access ([§2.1](#21-egress-sandbox-outbound-access-log-hostname--ip)) and policy audit ([§2.2](#22-egress-policy-initialization-and-allowdeny-rule-changes)) are **default on** at **info**; they are **not** controlled by separate `OPENSANDBOX_*` env vars in this OSEP.

Optionally read some of these from existing config or flags and allow environment variables to override.

## Test Plan

- **Unit tests**
  - Metrics: Create a MeterProvider with an in-memory or mock exporter, run business logic, assert exported metric count and key attributes; for **egress**, assert **`sandbox_id`** (and optional extras) on metric attributes and on Resource when **`OPENSANDBOX_EGRESS_SANDBOX_ID`** is set.
  - Logging: Assert structured log records contain expected fields (e.g. `sandbox_id` on egress where applicable).
  - Egress outbound access: Issue DNS queries (allow / forward-error paths); assert **info**-level log records contain `target.host`, `target.ips`, and/or `error` when applicable.
  - Egress policy: Assert `egress.loaded` after startup with expected rule summary; POST/PATCH policy and assert `egress.updated` (or `egress.update_failed` on invalid body).
- **Integration tests**
  - Start execd/egress/ingress with OTLP endpoint pointing at a test Collector or mock; send HTTP requests and trigger execution/DNS/policy/proxy; verify OTLP payloads contain expected **metrics** (and **logs** where OTLP logging is implemented; egress: validate **metrics** + stdout structured logs).
- **Configuration**
  - When `OTEL_EXPORTER_OTLP_*` is unset, no connection is made and no error is raised.
  - Environment variables override config file where applicable.

Acceptance: With OTLP enabled, Prometheus or the backend shows all execd, egress, and ingress **metrics** listed above (per component); **egress** structured logs appear on **stdout** with fields per [§2](#2-logging) (OTLP log export not required for egress). Egress **default** outbound access logs include `target.host`, `target.ips`, and/or `error` when applicable, without adding hostname as a metric label. Egress emits `egress.loaded` for initial policy and `egress.updated` after successful rule changes, with **`rules`** semantics per [§2.2](#22-egress-policy-initialization-and-allowdeny-rule-changes). **No OpenTelemetry Tracing** is required for acceptance.

## Drawbacks

- Additional dependencies and binary size (OpenTelemetry SDK and OTLP exporters for metrics; OTLP logs exporters only where implemented—egress is metrics-only OTLP).
- Under high QPS, metrics and log export add some CPU/memory cost; per-query egress outbound logs at **info** can be **high volume**—operators may lower log level or tune pipelines; aggregation dimensions control metric cardinality.

## Infrastructure Needed

- **Go dependencies**
  - `go.opentelemetry.io/otel`
  - `go.opentelemetry.io/otel/sdk`
  - `go.opentelemetry.io/otel/exporters/otlp/...` (metrics and logs, HTTP or gRPC as needed)
   - **No** `go.opentelemetry.io/contrib/.../otelhttp` or `otelgin` **required** for tracing; HTTP metrics may use manual instrumentation or minimal middleware that only records metrics.
- **Runtime**
  - For direct OTLP: a reachable OTLP endpoint (e.g., OpenTelemetry Collector, or an OTLP-capable backend for metrics/logs).
  - For “no export” mode: no extra infrastructure.

## Upgrade & Migration Strategy

- **Backward compatibility**: No changes to existing HTTP metric endpoints or Logger interface; only new initialization and optional env vars. With OpenTelemetry unconfigured, behavior is unchanged.
- **Rollout**
  1. Ship initialization and config code with OTLP endpoint unset (noop).
  2. Enable OTLP in test; verify metrics and logs.
  3. Add metric and log instrumentation in execd/egress/ingress handlers.
  4. Enable in production and tune endpoint as needed.
- **Rollback**: Unset or clear `OTEL_EXPORTER_OTLP_*` to stop export; no code change required.
