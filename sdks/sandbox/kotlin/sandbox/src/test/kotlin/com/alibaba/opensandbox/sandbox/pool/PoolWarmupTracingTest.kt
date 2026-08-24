/*
 * Copyright 2026 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package com.alibaba.opensandbox.sandbox.pool

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec
import com.alibaba.opensandbox.sandbox.domain.pool.PooledSandboxCreator
import com.alibaba.opensandbox.sandbox.domain.pool.SandboxPreparer
import com.alibaba.opensandbox.sandbox.infrastructure.pool.InMemoryPoolStateStore
import com.alibaba.opensandbox.sandbox.internal.PoolTracer
import io.mockk.every
import io.mockk.mockk
import io.opentelemetry.api.GlobalOpenTelemetry
import io.opentelemetry.api.common.AttributeKey
import io.opentelemetry.api.trace.propagation.W3CTraceContextPropagator
import io.opentelemetry.context.propagation.ContextPropagators
import io.opentelemetry.sdk.OpenTelemetrySdk
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter
import io.opentelemetry.sdk.trace.SdkTracerProvider
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor
import okhttp3.Request
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.slf4j.MDC
import java.time.Duration
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicReference

class PoolWarmupTracingTest {
    private var exporter: InMemorySpanExporter? = null
    private var openTelemetry: OpenTelemetrySdk? = null

    @AfterEach
    fun tearDown() {
        openTelemetry?.close()
        openTelemetry = null
        exporter = null
        GlobalOpenTelemetry.resetForTest()
    }

    private fun installSdkTracerProvider(): InMemorySpanExporter {
        val spanExporter = InMemorySpanExporter.create()
        val sdkTracerProvider =
            SdkTracerProvider.builder()
                .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
                .build()
        val otel =
            OpenTelemetrySdk.builder()
                .setTracerProvider(sdkTracerProvider)
                // Default propagators are noop; set W3C so traceparent injection
                // (HttpClientProvider) can be asserted.
                .setPropagators(ContextPropagators.create(W3CTraceContextPropagator.getInstance()))
                .build()
        GlobalOpenTelemetry.set(otel)
        exporter = spanExporter
        openTelemetry = otel
        return spanExporter
    }

    @Test
    fun `warmup emits a full span tree with drill-down attributes when tracing enabled`() {
        val spanExporter = installSdkTracerProvider()
        val capturedTraceId = AtomicReference<String>()
        val capturedSpanId = AtomicReference<String>()

        val store = InMemoryPoolStateStore()
        val pool =
            SandboxPool.builder()
                .poolName("trace-pool")
                .ownerId("trace-owner")
                .maxIdle(1)
                .warmupConcurrency(1)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().enableTracing().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(
                    PooledSandboxCreator {
                        mockk<Sandbox>(relaxed = true).also { sandbox ->
                            every { sandbox.id } returns "warmup-trace-1"
                        }
                    },
                ).warmupHealthCheck { true }
                .warmupSandboxPreparer(
                    SandboxPreparer {
                        capturedTraceId.set(MDC.get(PoolTracer.MDC_TRACE_ID))
                        capturedSpanId.set(MDC.get(PoolTracer.MDC_SPAN_ID))
                    },
                )
                .warmupPostPrepareHealthCheck { true }
                .drainTimeout(Duration.ofSeconds(2))
                .build()

        pool.start()
        try {
            awaitCondition { store.snapshotCounters("trace-pool").idleCount == 1 }
            val spans = spanExporter.finishedSpanItems
            val root = spans.single { it.name == PoolTracer.WARMUP_ROOT_SPAN }
            assertEquals("trace-pool", root.attributes[AttributeKey.stringKey(PoolTracer.ATTR_POOL_NAME)])
            assertEquals("trace-owner", root.attributes[AttributeKey.stringKey(PoolTracer.ATTR_POOL_OWNER)])
            assertEquals(1L, root.attributes[AttributeKey.longKey(PoolTracer.ATTR_POOL_RUN_GENERATION)])
            assertEquals(1L, root.attributes[AttributeKey.longKey(PoolTracer.ATTR_POOL_LEADER_EPOCH)])
            assertEquals("warmup-trace-1", root.attributes[AttributeKey.stringKey(PoolTracer.ATTR_SANDBOX_ID)])
            assertEquals("ubuntu:22.04", root.attributes[AttributeKey.stringKey(PoolTracer.ATTR_SANDBOX_IMAGE)])
            assertEquals("success", root.attributes[AttributeKey.stringKey(PoolTracer.ATTR_RESULT)])

            // MDC must expose the same trace while warmup code runs.
            assertEquals(root.traceId, capturedTraceId.get())
            assertEquals(root.spanId, capturedSpanId.get())

            val names = spans.map { it.name }.toSet()
            assertTrue(names.contains(PoolTracer.WARMUP_CREATE_SPAN))
            assertTrue(names.contains(PoolTracer.WARMUP_READINESS_CHECK_SPAN))
            assertTrue(names.contains(PoolTracer.WARMUP_PREPARE_SPAN))
            assertTrue(names.contains(PoolTracer.WARMUP_POST_PREPARE_CHECK_SPAN))
            assertTrue(names.contains(PoolTracer.WARMUP_RENEW_SPAN))
            assertTrue(names.contains(PoolTracer.WARMUP_COMMIT_SPAN))

            // All spans share one trace; phase spans are sequential siblings
            // under the root (each phase duration stands alone for drill-down),
            // and commit re-attaches to the root on the scheduler thread.
            spans.forEach { span ->
                assertEquals(root.traceId, span.traceId, "all spans must share the warmup trace id")
            }
            val create = spans.single { it.name == PoolTracer.WARMUP_CREATE_SPAN }
            val readiness = spans.single { it.name == PoolTracer.WARMUP_READINESS_CHECK_SPAN }
            val prepare = spans.single { it.name == PoolTracer.WARMUP_PREPARE_SPAN }
            val postPrepare = spans.single { it.name == PoolTracer.WARMUP_POST_PREPARE_CHECK_SPAN }
            val renew = spans.single { it.name == PoolTracer.WARMUP_RENEW_SPAN }
            val commit = spans.single { it.name == PoolTracer.WARMUP_COMMIT_SPAN }
            assertEquals(root.spanId, create.parentSpanId)
            assertEquals(root.spanId, readiness.parentSpanId)
            assertEquals(root.spanId, prepare.parentSpanId)
            assertEquals(root.spanId, postPrepare.parentSpanId)
            assertEquals(root.spanId, renew.parentSpanId)
            assertEquals(root.spanId, commit.parentSpanId)

            // Root span is backdated to submission, so the trace covers the
            // queue wait before the create phase.
            assertTrue(root.startEpochNanos <= create.startEpochNanos)
            // Root start must be epoch wall-clock (not monotonic nanoTime):
            // within a minute of test start, and not some arbitrary boot-relative value.
            val testStartEpochNanos = System.currentTimeMillis() * 1_000_000L
            assertTrue(
                root.startEpochNanos <= testStartEpochNanos,
                "root start must be in the past relative to test start",
            )
            assertTrue(
                root.startEpochNanos >= testStartEpochNanos - Duration.ofMinutes(1).toNanos(),
                "root start must be epoch wall-clock near test start",
            )
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `failed warmup emits a failure root span without a commit span`() {
        val spanExporter = installSdkTracerProvider()
        val store = InMemoryPoolStateStore()
        val pool =
            SandboxPool.builder()
                .poolName("trace-fail-pool")
                .ownerId("trace-fail-owner")
                .maxIdle(1)
                .warmupConcurrency(1)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().enableTracing().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(
                    PooledSandboxCreator {
                        throw RuntimeException("create boom")
                    },
                ).warmupSkipHealthCheck()
                .drainTimeout(Duration.ofMillis(200))
                .build()

        pool.start()
        try {
            awaitCondition { pool.snapshot().failureCount >= 1 }
            awaitCondition { spanExporter.finishedSpanItems.any { it.name == PoolTracer.WARMUP_ROOT_SPAN } }
            val spans = spanExporter.finishedSpanItems
            val root = spans.single { it.name == PoolTracer.WARMUP_ROOT_SPAN }
            assertEquals("failure", root.attributes[AttributeKey.stringKey(PoolTracer.ATTR_RESULT)])
            assertEquals(
                1,
                root.events.count { it.name == "exception" },
                "failure must be recorded on the root span",
            )
            assertTrue(spans.none { it.name == PoolTracer.WARMUP_COMMIT_SPAN })
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `dropped warmup commit is traced as a failure`() {
        val spanExporter = installSdkTracerProvider()
        val store = LockLossOnCommitPoolStateStore()
        val pool =
            SandboxPool.builder()
                .poolName("trace-drop-pool")
                .ownerId("trace-drop-owner")
                .maxIdle(1)
                .warmupConcurrency(1)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().enableTracing().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(
                    PooledSandboxCreator {
                        mockk<Sandbox>(relaxed = true).also { sandbox ->
                            every { sandbox.id } returns "warmup-dropped-1"
                        }
                    },
                ).warmupSkipHealthCheck()
                .warmupSandboxPreparer(
                    SandboxPreparer {
                        store.failRenewPrimaryLock = true
                    },
                )
                .drainTimeout(Duration.ofSeconds(2))
                .build()

        pool.start()
        try {
            awaitCondition { spanExporter.finishedSpanItems.any { it.name == PoolTracer.WARMUP_ROOT_SPAN } }
            val spans = spanExporter.finishedSpanItems
            val root = spans.single { it.name == PoolTracer.WARMUP_ROOT_SPAN }
            assertEquals("failure", root.attributes[AttributeKey.stringKey(PoolTracer.ATTR_RESULT)])
            assertEquals(
                "warmup-lock-lost",
                root.attributes[AttributeKey.stringKey(PoolTracer.ATTR_DROP_REASON)],
            )
            assertNull(root.attributes[AttributeKey.stringKey(PoolTracer.ATTR_SANDBOX_ID)])
            assertTrue(
                spans.any { it.name == PoolTracer.WARMUP_COMMIT_SPAN },
                "commit phase must still be traced",
            )
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `warmup emits no spans when tracing is disabled`() {
        val spanExporter = installSdkTracerProvider()
        val store = InMemoryPoolStateStore()
        val pool =
            SandboxPool.builder()
                .poolName("no-trace-pool")
                .ownerId("no-trace-owner")
                .maxIdle(1)
                .warmupConcurrency(1)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(
                    PooledSandboxCreator {
                        mockk<Sandbox>(relaxed = true).also { sandbox ->
                            every { sandbox.id } returns "no-trace-1"
                        }
                    },
                ).warmupSkipHealthCheck()
                .drainTimeout(Duration.ofSeconds(2))
                .build()

        pool.start()
        try {
            awaitCondition { store.snapshotCounters("no-trace-pool").idleCount == 1 }
            assertTrue(
                spanExporter.finishedSpanItems.isEmpty(),
                "no spans may be emitted when enableTracing is false",
            )
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `requests inject traceparent only when tracing is enabled and a span is current`() {
        val spanExporter = installSdkTracerProvider()
        val server = MockWebServer()
        try {
            server.enqueue(MockResponse().setResponseCode(204))
            server.enqueue(MockResponse().setResponseCode(204))
            server.enqueue(MockResponse().setResponseCode(204))
            val tracer = GlobalOpenTelemetry.get().tracerBuilder("test").build()
            val span = tracer.spanBuilder("test-span").startSpan()
            span.makeCurrent().use {
                HttpClientProvider(
                    ConnectionConfig.builder()
                        .domain(server.url("/").toString().removeSuffix("/"))
                        .enableTracing()
                        .build(),
                ).use { provider ->
                    provider.httpClient.newCall(Request.Builder().url(server.url("/tracing")).build()).execute()
                        .use { }
                    val recorded = server.takeRequest(1, TimeUnit.SECONDS)!!
                    val traceparent = recorded.getHeader("traceparent")
                    assertNotNull(traceparent, "traceparent must be injected for an active span")
                    assertTrue(traceparent!!.startsWith("00-"), "traceparent must be W3C v00 format")
                    assertTrue(traceparent.contains(span.spanContext.traceId), "traceparent must carry the trace id")
                }
            }
            span.end()

            // No active span -> no injection, even with tracing enabled.
            HttpClientProvider(
                ConnectionConfig.builder()
                    .domain(server.url("/").toString().removeSuffix("/"))
                    .enableTracing()
                    .build(),
            ).use { provider ->
                provider.httpClient.newCall(Request.Builder().url(server.url("/no-span")).build()).execute().use { }
                val recorded = server.takeRequest(1, TimeUnit.SECONDS)!!
                assertNull(recorded.getHeader("traceparent"))
            }

            // Tracing disabled -> no injection even with an active span.
            val disabledSpan = tracer.spanBuilder("disabled-span").startSpan()
            disabledSpan.makeCurrent().use {
                HttpClientProvider(
                    ConnectionConfig.builder()
                        .domain(server.url("/").toString().removeSuffix("/"))
                        .build(),
                ).use { provider ->
                    provider.httpClient.newCall(Request.Builder().url(server.url("/disabled")).build()).execute()
                        .use { }
                    val recorded = server.takeRequest(1, TimeUnit.SECONDS)!!
                    assertNull(recorded.getHeader("traceparent"))
                }
            }
            disabledSpan.end()
            assertTrue(spanExporter.finishedSpanItems.isNotEmpty())
        } finally {
            server.shutdown()
        }
    }

    private fun awaitCondition(
        timeout: Duration = Duration.ofSeconds(5),
        condition: () -> Boolean,
    ) {
        val deadline = System.nanoTime() + timeout.toNanos()
        while (System.nanoTime() < deadline) {
            if (condition()) return
            Thread.sleep(20)
        }
        throw AssertionError("condition not met within $timeout")
    }

    /**
     * In-memory store whose primary-lock renewal starts failing once a warmup
     * preparer sets [failRenewPrimaryLock], so the commit path drops the
     * warmed sandbox with `warmup-lock-lost`.
     */
    private class LockLossOnCommitPoolStateStore(
        private val delegate: InMemoryPoolStateStore = InMemoryPoolStateStore(),
    ) : com.alibaba.opensandbox.sandbox.domain.pool.PoolStateStore by delegate {
        @Volatile
        var failRenewPrimaryLock: Boolean = false

        override fun renewPrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean {
            if (failRenewPrimaryLock) return false
            return delegate.renewPrimaryLock(poolName, ownerId, ttl)
        }
    }
}
