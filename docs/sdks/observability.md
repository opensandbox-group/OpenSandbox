---
title: SDK Observability
description: Configure SDK creation metrics and OpenTelemetry pool warmup traces.
---

# SDK Observability

Use creation metrics to track sandbox startup latency and pool warmup traces to locate slow phases. They have separate controls and export paths:

| Signal | Use it to | SDK support | Default | Export path |
| --- | --- | --- | --- | --- |
| [Pool warmup traces](#pool-warmup-tracing) | Inspect creation, readiness, preparation, renewal, and idle publication | Python, JavaScript/TypeScript, Kotlin/Java | Off; enable tracing in the connection config | Application-owned OpenTelemetry provider and exporter |
| [Creation metrics](#creation-metrics) | Measure sandbox create latency and success | All five SDKs | On; disable through config or `OPENSANDBOX_DISABLE_METRICS=1` | SDK posts to the lifecycle server, which exports metrics when `[otel]` is enabled |

OpenTelemetry (OTel) provides the tracing and metrics infrastructure; enabling tracing does not enable or disable creation metrics. For remote sandbox logs and events, see the [diagnostics API](/api/#diagnostics).

## Pool warmup tracing

The Python, JavaScript/TypeScript, and Kotlin/Java SDKs can emit [OpenTelemetry](https://opentelemetry.io/) traces for the client-side `SandboxPool` warmup path. Each warmup task becomes one trace that covers the full lifecycle — from the moment the reconcile loop submits the task until the warmed sandbox is committed to the idle buffer — with per-phase spans so you can find the actual warmup bottleneck.

Tracing is **opt-in** (`enable_tracing=True` in Python or `enableTracing: true` in JavaScript, or `.enableTracing(true)` on the JVM) and **best-effort**: without an OpenTelemetry SDK + exporter in the application, all span calls are no-ops and nothing is exported. Tracing never affects pool behavior.

### SDK support

| SDK | Pool warmup tracing |
| --- | --- |
| Python async/sync | Phase spans, sandbox identity, terminal classification, readiness counters |
| JavaScript/TypeScript | Phase spans, pool identity, success/failure and error type |
| Kotlin/Java | Phase spans, sandbox identity, terminal classification, readiness counters, SLF4J MDC |
| Go / C# | No built-in pool warmup tracing |

This guide describes the default branch. Check your installed SDK's `ConnectionConfig` for `enable_tracing` / `enableTracing` before enabling it. Tracing defaults to **off**, independently of [create-latency telemetry](/sdks/observability#creation-metrics), which defaults to **on**.

### Enabling tracing

#### 1. Add an OpenTelemetry SDK + exporter to your application

These SDKs depend only on the OpenTelemetry API (no-op by default). To actually export traces you bring your own SDK and exporter. For Python:

```bash
pip install opentelemetry-sdk opentelemetry-exporter-otlp-proto-http
```

For Node.js, install an OpenTelemetry SDK and exporter compatible with your application. See the [OpenTelemetry Node.js setup](https://opentelemetry.io/docs/languages/js/getting-started/nodejs/).

For Kotlin/Java:

```kotlin
dependencies {
    implementation("io.opentelemetry:opentelemetry-api:1.51.0")
    implementation("io.opentelemetry:opentelemetry-sdk:1.51.0")
    implementation("io.opentelemetry:opentelemetry-exporter-otlp:1.51.0")
}
```

#### 2. Configure the global OpenTelemetry provider

Warmup spans use the language's global provider. Configure it at application startup, before creating or starting the pool. In JavaScript, register the provider, async context manager, and W3C propagator with the global `@opentelemetry/api` instance (a Node.js OpenTelemetry SDK can configure these together). For Python, use `opentelemetry.trace.set_tracer_provider(...)`; for Kotlin/Java, configure `GlobalOpenTelemetry`, for example:

```java
import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.sdk.OpenTelemetrySdk;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.export.BatchSpanProcessor;
import io.opentelemetry.exporter.otlp.trace.OtlpGrpcSpanExporter;

SdkTracerProvider tracerProvider = SdkTracerProvider.builder()
    .addSpanProcessor(BatchSpanProcessor.create(
        OtlpGrpcSpanExporter.builder()
            .setEndpoint("http://otel-collector:4317")
            .build()))
    .build();

OpenTelemetrySdk sdk = OpenTelemetrySdk.builder()
    .setTracerProvider(tracerProvider)
    .build();

GlobalOpenTelemetry.set(sdk);
```

::: tip Propagators
`OpenTelemetrySdk.builder()` defaults to **noop propagators**. If you want the SDK to inject the W3C `traceparent` header into lifecycle requests (so the lifecycle server can join the same trace once it supports tracing), configure W3C propagation explicitly:

```java
.setPropagators(ContextPropagators.create(W3CTraceContextPropagator.getInstance()))
```
:::

::: tip Sampling
To keep trace volume bounded, use a sampling strategy such as `parentbased_traceidratio(0.1)` on the `SdkTracerProvider`. Trace-id-ratio sampling keeps client and server spans consistent for the same warmup.
:::

#### 3. Turn tracing on for the pool

JavaScript / TypeScript:

```ts
import { ConnectionConfig } from "@alibaba-group/opensandbox";

const config = new ConnectionConfig({ enableTracing: true });
// Pass config as connectionConfig when constructing SandboxPool.
```

Python:

```python
from opensandbox.config import ConnectionConfig

config = ConnectionConfig(enable_tracing=True)
```

Kotlin/Java:

```java
ConnectionConfig config = ConnectionConfig.builder()
    .enableTracing(true)
    .build();

SandboxPool pool = SandboxPool.builder()
    .poolName("demo-pool")
    .maxIdle(3)
    .stateStore(new InMemoryPoolStateStore())
    .connectionConfig(config)
    .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
    .build();
```

Pass this connection config to the pool. The SDK flag defaults to `false`; OpenTelemetry exporter and sampling configuration remain application-owned. Shut down the pool before flushing and closing the application tracer provider.

### What is traced

Each warmup task produces **one trace** with a root span and six possible phase types (siblings under the root, so each phase duration stands alone for comparison). Each readiness stage is summarized by one span across all of its delayed attempts; optional stages are absent when they are not configured:

| Span name | Covers |
|-----------|--------|
| `pool.warmup` (root) | Warmup task, from admission through completion. Python/JVM backdate it to submission time to include queue wait |
| `pool.warmup.create` | Sandbox creator invocation. The built-in lifecycle path makes one HTTP attempt; readiness is no longer part of this span |
| `pool.warmup.readiness` | Complete pre-prepare readiness stage (`warmupHealthCheck` or `ping`), including all delayed attempts |
| `pool.warmup.prepare` | The single invocation of `warmupSandboxPreparer` (user init script / setup work) |
| `pool.warmup.post_prepare_readiness` | Complete optional post-prepare validation stage, including all delayed attempts |
| `pool.warmup.renew` | TTL renewal right before committing the sandbox |
| `pool.warmup.commit` | Primary-lock renewal + `putIdle` against the state store |

#### Attributes by language

All three SDKs emit `pool.name`, `pool.owner`, `pool.run.generation`, and `pool.leader.epoch`. JavaScript currently adds `warmup.result` (`success` or `failure`) and `warmup.error.type`; it does not emit the sandbox identity, terminal-stage classification, or readiness-attempt counters below. Its success flag alone does not prove that the sandbox was committed to idle.

The following richer root attributes apply to **Python and Kotlin/Java**:

| Attribute | Value |
|-----------|-------|
| `pool.name` | Pool name |
| `pool.owner` | Pool owner id |
| `pool.run.generation` | Pool run generation |
| `pool.leader.epoch` | Leader epoch captured when this warmup was admitted |
| `sandbox.id` | Sandbox id when creation progressed far enough to obtain one |
| `sandbox.image` | Creation image |
| `warmup.stage` | Terminal stage: `admission`, `create`, `readiness`, `prepare`, `post_prepare_readiness`, `renew`, or `commit` |
| `warmup.result` | `success`, `failure`, `dropped`, or `cancelled` |
| `warmup.reason` | Stable terminal reason when the result is not successful |
| `warmup.error.category` | Stable error category such as `rate_limit`, `http_4xx`, `http_5xx`, `timeout`, `connection`, `callback`, or `state_store` |
| `warmup.error.type` | Exception class when an error is available |

Python/JVM readiness summary spans additionally expose `warmup.health.attempt_count`, `warmup.health.false_count`, `warmup.health.exception_count`, and `warmup.scheduler.delay_ms`. Failures are recorded with `recordException` on the affected phase span. The root span keeps the classified terminal stage, result, reason, and OpenTelemetry error status without duplicating the phase exception event.

::: warning Development snapshot attribute migration
Earlier development snapshots used the unnamespaced `result` and `drop.reason` attributes. The supported schema uses `warmup.result` and `warmup.reason` consistently across traces and structured logs. The old keys are not emitted in parallel; update any dashboards created against a development snapshot.
:::

### Correlating logs to traces

In Kotlin/Java, while a warmup trace is in progress, the pool publishes trace IDs to the SLF4J [MDC](https://www.slf4j.org/api/org/slf4j/MDC.html):

| MDC key | Value |
|---------|-------|
| `trace_id` | Current trace id |
| `span_id` | Current span id |

MDC requires a real SLF4J provider (logback, log4j2, ...). Add the keys to your log pattern once, and every pool log line carries the trace context:

```xml
<pattern>%d %-5level [%thread] %logger{36} trace_id=%X{trace_id} span_id=%X{span_id} - %msg%n</pattern>
```

### Querying traces

The trace id is random, so a warmup trace cannot be looked up "by pool name" directly. The reliable paths are:

1. **JVM log correlation.** With MDC configured, the pool logs `pool_name` and `sandbox_id` on its warmup lines (e.g. `Pool warmup sandbox entered idle`). Search your logs for a `sandbox_id` — the matching log lines carry `trace_id`, which you can open directly in your trace backend.
2. **Attribute query in the trace backend.** Filter spans by time window and attribute, e.g. TraceQL `{ span.pool.name = "demo-pool" }` (Grafana Tempo), or Jaeger tag search on `pool.name=...`. Backends that derive metrics from spans (Tempo metrics, Datadog span analytics) let you look at `pool.warmup` duration percentiles per `pool.name` first, then drill into slow traces.
3. **Trace-id-ratio sampling.** With sampled traces, `trace_id` in logs and the backend are consistent for the same warmup.

#### Bottleneck drill-down

The readiness counters and backdated queue timing below apply to Python/JVM. For JavaScript, compare phase durations and the four pool identity attributes.

```
pool.warmup root duration (p50/p95/p99) per pool.name
  └─ phase spans: create / readiness / prepare / post_prepare_readiness / renew / commit
       └─ single trace: root start gap = queue wait, then each phase duration
```

| Symptom | Likely cause |
|---------|--------------|
| Long gap before `pool.warmup.create` | Create tasks waiting for an executor thread; compare `warmupCreateQps` with create latency |
| Long gap between create and the first readiness span | Expected `warmupHealthCheckInitialDelay`, or delayed-stage capacity exhausted because `warmupConcurrency` is too low |
| `pool.warmup.create` slow | Lifecycle create API slow (for example image pull / execd startup) |
| Slow `pool.warmup.readiness` with a high `warmup.health.attempt_count` | Sandbox startup or the configured readiness predicate is the bottleneck |
| `pool.warmup.prepare` slow | Your `warmupSandboxPreparer` work is the bottleneck |
| Slow `pool.warmup.post_prepare_readiness` with a high attempt count | Prepared service is not yet healthy, or its validation predicate is slow |
| `pool.warmup.renew` slow | Lifecycle API TTL renewal |
| `pool.warmup.commit` slow | State-store lease renewal or idle publication (for example Redis round-trips) |

## Creation metrics

OpenSandbox SDKs report sandbox creation latency to the configured lifecycle server by default. This reporting is separate from opt-in [pool warmup tracing](/sdks/observability#pool-warmup-tracing). Reporting is best-effort: failures never affect `Sandbox.create`, and the payload contains no user content.

### Requirements

The `POST /v1/metrics/events` endpoint and the SDK reporters described below require the following minimum versions. Older SDKs simply do not emit events; older servers reject unknown routes with `404`, which the SDK swallows silently (see [Version skew](#version-skew) below).

| Component | Minimum version |
|-----------|-----------------|
| Server (`opensandbox-server`) | `0.2.2` |
| Python SDK (`opensandbox`) | `0.1.15` |
| JavaScript / TypeScript SDK (`@alibaba-group/opensandbox`) | `0.1.11` |
| Go SDK (`github.com/alibaba/OpenSandbox/sdks/sandbox/go`) | `1.0.5` |
| C# SDK (`Alibaba.OpenSandbox`) | `0.1.5` |
| Kotlin / Java SDK (`com.alibaba.opensandbox:sandbox`) | `1.0.17` |

#### Version skew

Reporting is fire-and-forget in every SDK: the POST runs on a background task/thread, and reporting failures are ignored. Debug logging varies by SDK; JavaScript, for example, does not log these failures. This means you can upgrade the SDK and the server independently:

- **New SDK, old server (`< 0.2.2`)**: the server returns `404` for `/v1/metrics/events`. The SDK ignores the response. `Sandbox.create` behavior is unchanged and no user-visible error is raised. A debug message may be emitted depending on the SDK.
- **Old SDK, new server**: the SDK does not emit events. The server histogram simply records nothing for that client.
- **Network errors, TLS failures, timeouts**: same behavior as the `404` case — swallowed, `Sandbox.create` unaffected.

### What is sent

After create succeeds or fails, the SDK fire-and-forget posts to `POST /v1/metrics/events`:

```json
{
  "eventType": "sandbox.create",
  "sandboxId": "sbx_...",
  "image": "python:3.12",
  "createDurationMs": 1842,
  "success": true
}
```

- `sandboxId` / `image` may be omitted when create fails early.
- SDK language and version come from the HTTP `User-Agent` header (for example `OpenSandbox-Python-SDK/0.1.15`), not from body fields.

The server accepts the event with `204` and, when `[otel]` is enabled, records an OTEL histogram. See [server configuration](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md#otel).

### When it runs

| SDK | Trigger |
|-----|---------|
| Python (async + sync) | After `Sandbox.create` / sync create completes or raises |
| JavaScript / TypeScript | After `Sandbox.create` completes or fails |
| Go | After `CreateSandbox` completes or fails |
| C# | After `Sandbox.CreateAsync` completes or fails |
| Kotlin | After standalone `Sandbox.builder()...build()` or a pool direct-create fallback completes or fails |

::: info Kotlin staged pool warmup
Kotlin staged warmup deliberately does **not** emit the legacy `sandbox.create` event. Its create phase returns before readiness polling, optional preparation, post-prepare validation, renewal, and idle commit, so reporting that partial phase as the existing end-to-end create histogram would give the metric a different meaning from standalone create.

This exclusion applies only to staged warmup. Standalone create and pool direct-create fallback keep reporting normally. Use the pool's structured summary logs and optional [warmup tracing](/sdks/observability#pool-warmup-tracing) to observe the complete staged-warmup lifecycle.
:::

### How to disable

Default is on. Opt out with either:

1. Environment variable (all SDKs):

```bash
export OPENSANDBOX_DISABLE_METRICS=1
```

2. Connection config field:

::: code-group

```python [Python]
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig

config = ConnectionConfig(disable_metrics=True)
sandbox = await Sandbox.create("python:3.12", connection_config=config)
```

```typescript [JavaScript]
import { ConnectionConfig, Sandbox } from "@alibaba-group/opensandbox";

const connectionConfig = new ConnectionConfig({ disableMetrics: true });
const sandbox = await Sandbox.create({
  image: "python:3.12",
  connectionConfig,
});
```

```go [Go]
cfg := opensandbox.ConnectionConfig{DisableMetrics: true}
sandbox, err := opensandbox.CreateSandbox(ctx, cfg, opensandbox.SandboxCreateOptions{
    Image: "python:3.12",
})
```

```csharp [C#]
using OpenSandbox;
using OpenSandbox.Config;

var connectionConfig = new ConnectionConfig(new ConnectionConfigOptions
{
    DisableMetrics = true,
});
var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
{
    Image = "python:3.12",
    ConnectionConfig = connectionConfig,
});
```

```kotlin [Kotlin]
import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig

val connectionConfig = ConnectionConfig.builder()
    .disableMetrics(true)
    .build()
val sandbox = Sandbox.builder()
    .image("python:3.12")
    .connectionConfig(connectionConfig)
    .build()
```

:::

These requests go to your configured lifecycle server. Use opt-out when you do not want this additional traffic or create-latency data recorded there.
