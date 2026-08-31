---
title: SDK Tracing (Pool Warmup)
description: How to enable OpenTelemetry tracing for the Kotlin SDK pool warmup path, what spans are produced, and how to query and drill down into warmup traces.
---

# SDK Tracing (Pool Warmup)

The Kotlin/Java SDK (`com.alibaba.opensandbox:sandbox`) can emit
[OpenTelemetry](https://opentelemetry.io/) traces for the client-side
`SandboxPool` warmup path. Each warmup task becomes one trace that covers the
full lifecycle — from the moment the reconcile loop submits the task until the
warmed sandbox is committed to the idle buffer — with per-phase spans so you
can find the actual warmup bottleneck.

Tracing is **opt-in** (`enableTracing(true)`) and **best-effort**: without an
OpenTelemetry SDK + exporter on the application classpath, all span calls are
no-ops and nothing is exported. Tracing never throws and never affects pool
behavior.

## Requirements

| Component | Minimum version |
|-----------|-----------------|
| Kotlin / Java SDK (`com.alibaba.opensandbox:sandbox`) | `1.0.19` |

Only the Kotlin SDK emits these traces today; the other language SDKs do not
yet support `enableTracing`.

## Enabling tracing

### 1. Add an OpenTelemetry SDK + exporter to your application

The SDK depends only on `opentelemetry-api` (no-op by default). To actually
export traces you bring your own SDK and exporter, for example OTLP over HTTP:

```kotlin
dependencies {
    implementation("io.opentelemetry:opentelemetry-api:1.51.0")
    implementation("io.opentelemetry:opentelemetry-sdk:1.51.0")
    implementation("io.opentelemetry:opentelemetry-exporter-otlp:1.51.0")
}
```

### 2. Configure a global `OpenTelemetry` instance

Warmup spans use the global instance (`GlobalOpenTelemetry`). Configure it at
application startup, e.g. with `OpenTelemetrySdk`:

```java
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
`OpenTelemetrySdk.builder()` defaults to **noop propagators**. If you want the
SDK to inject the W3C `traceparent` header into lifecycle requests (so the
lifecycle server can join the same trace once it supports tracing), configure
W3C propagation explicitly:

```java
.setPropagators(ContextPropagators.create(W3CTraceContextPropagator.getInstance()))
```
:::

::: tip Sampling
To keep trace volume bounded, use a sampling strategy such as
`parentbased_traceidratio(0.1)` on the `SdkTracerProvider`. Trace-id-ratio
sampling keeps client and server spans consistent for the same warmup.
:::

### 3. Turn tracing on for the pool

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

That is all. No environment variables are involved; `enableTracing` defaults
to `false`.

## What is traced

Each warmup task produces **one trace** with a root span and six possible phase
types (siblings under the root, so each phase duration stands alone for
comparison). Each readiness stage is summarized by one span across all of its
delayed attempts; optional stages are absent when they are not configured:

| Span name | Covers |
|-----------|--------|
| `pool.warmup` (root) | Task submission → sandbox committed to idle. Backdated to submission time, so the queue wait before the first phase is visible as the gap before the first child span |
| `pool.warmup.create` | Sandbox creator invocation. The built-in lifecycle path makes one HTTP attempt; readiness is no longer part of this span |
| `pool.warmup.readiness` | Complete pre-prepare readiness stage (`warmupHealthCheck` or `ping`), including all delayed attempts |
| `pool.warmup.prepare` | The single invocation of `warmupSandboxPreparer` (user init script / setup work) |
| `pool.warmup.post_prepare_readiness` | Complete optional post-prepare validation stage, including all delayed attempts |
| `pool.warmup.renew` | TTL renewal right before committing the sandbox |
| `pool.warmup.commit` | Primary-lock renewal + `putIdle` against the state store |

Root span attributes (these are your drill-down dimensions):

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

Readiness summary spans additionally expose
`warmup.health.attempt_count`, `warmup.health.false_count`,
`warmup.health.exception_count`, and `warmup.scheduler.delay_ms`. Failures are
recorded with `recordException` on the affected phase span. The root span keeps
the classified terminal stage, result, reason, and OpenTelemetry error status
without duplicating the phase exception event.

::: warning Development snapshot attribute migration
Earlier development snapshots used the unnamespaced `result` and
`drop.reason` attributes. The supported schema uses `warmup.result` and
`warmup.reason` consistently across traces and structured logs. The old keys
are not emitted in parallel; update any dashboards created against a
development snapshot.
:::

## Correlating logs to traces

While a warmup trace is in progress, the pool publishes the trace ids to the
SLF4J [MDC](https://www.slf4j.org/api/org/slf4j/MDC.html):

| MDC key | Value |
|---------|-------|
| `trace_id` | Current trace id |
| `span_id` | Current span id |

MDC requires a real SLF4J provider (logback, log4j2, ...). Add the keys to
your log pattern once, and every pool log line carries the trace context:

```xml
<pattern>%d %-5level [%thread] %logger{36} trace_id=%X{trace_id} span_id=%X{span_id} - %msg%n</pattern>
```

## Querying traces

The trace id is random, so a warmup trace cannot be looked up "by pool name"
directly. The reliable paths are:

1. **Log correlation (recommended).** The pool already logs `pool_name` and
   `sandbox_id` on its warmup lines (e.g. `Pool warmup sandbox entered idle`).
   Search your logs for a `sandbox_id` — the matching log lines carry
   `trace_id`, which you can open directly in your trace backend.
2. **Attribute query in the trace backend.** Filter spans by time window and
   attribute, e.g. TraceQL `{ span.pool.name = "demo-pool" }` (Grafana Tempo),
   or Jaeger tag search on `pool.name=...`. Backends that derive metrics from
   spans (Tempo metrics, Datadog span analytics) let you look at
   `pool.warmup` duration percentiles per `pool.name` first, then drill into
   slow traces.
3. **Trace-id-ratio sampling.** With sampled traces, `trace_id` in logs and
   the backend are consistent for the same warmup.

### Bottleneck drill-down

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
| `pool.warmup.renew` / `pool.warmup.commit` slow | State store (e.g. Redis) round-trips |
