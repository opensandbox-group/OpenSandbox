/*
 * Copyright 2026 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package com.alibaba.opensandbox.sandbox.internal

import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import io.opentelemetry.api.GlobalOpenTelemetry
import io.opentelemetry.api.trace.Span
import io.opentelemetry.api.trace.Tracer
import org.slf4j.MDC
import java.util.concurrent.TimeUnit

/**
 * Best-effort OpenTelemetry tracing for the client-side pool warmup path.
 *
 * Tracing is opt-in via [ConnectionConfig.enableTracing]. When enabled, each
 * warmup task produces one trace rooted at a [WARMUP_ROOT_SPAN] span with
 * per-phase child spans (create / prepare / renew / commit). The root span
 * starts at task submission time so queue-waiting time is visible as the gap
 * before the first child span.
 *
 * While a warmup trace is current, `trace_id` and `span_id` are published to
 * the SLF4J [MDC] so application logs emitted by the pool (which already
 * carry `pool_name` / `sandbox_id`) can be correlated back to a trace —
 * search logs by sandbox_id to obtain the trace_id, then open it in the
 * trace backend.
 *
 * All span/MDC calls are best-effort and MUST NOT surface any exception to
 * the caller: without an OpenTelemetry SDK on the classpath every call is a
 * no-op, and MDC access can fail under unusual logging setups.
 */
internal class PoolTracer private constructor(
    private val tracer: Tracer?,
) {
    val enabled: Boolean
        get() = tracer != null

    /**
     * Starts the root span of one warmup trace, backdated to
     * [submittedEpochNanos] (epoch wall-clock, see
     * [WarmupTrace.endSuccess]) so the queue-wait time is part of the trace.
     * Returns null when tracing is disabled; the caller then runs without
     * spans.
     */
    fun startWarmupRoot(
        poolName: String,
        ownerId: String,
        runGeneration: Long,
        submittedEpochNanos: Long,
    ): WarmupTrace? {
        val t = tracer ?: return null
        val root =
            t.spanBuilder(WARMUP_ROOT_SPAN)
                .setAttribute(ATTR_POOL_NAME, poolName)
                .setAttribute(ATTR_POOL_OWNER, ownerId)
                .setAttribute(ATTR_POOL_RUN_GENERATION, runGeneration)
                .setStartTimestamp(submittedEpochNanos, TimeUnit.NANOSECONDS)
                .startSpan()
        return WarmupTrace(root)
    }

    /**
     * Runs [block] under a child span of the currently-current span (the
     * warmup root). No-op span when tracing is disabled.
     */
    internal inline fun <T> withPhaseSpan(
        spanName: String,
        crossinline block: () -> T,
    ): T {
        val t = tracer ?: return block()
        val span = t.spanBuilder(spanName).startSpan()
        val scope = span.makeCurrent()
        return try {
            block()
        } finally {
            safeClose(scope)
            span.end()
        }
    }

    companion object {
        const val WARMUP_ROOT_SPAN = "pool.warmup"
        const val WARMUP_CREATE_SPAN = "pool.warmup.create"
        const val WARMUP_PREPARE_SPAN = "pool.warmup.prepare"
        const val WARMUP_RENEW_SPAN = "pool.warmup.renew"
        const val WARMUP_COMMIT_SPAN = "pool.warmup.commit"

        const val MDC_TRACE_ID = "trace_id"
        const val MDC_SPAN_ID = "span_id"

        const val ATTR_POOL_NAME = "pool.name"
        const val ATTR_POOL_OWNER = "pool.owner"
        const val ATTR_POOL_RUN_GENERATION = "pool.run.generation"
        const val ATTR_SANDBOX_ID = "sandbox.id"
        const val ATTR_SANDBOX_IMAGE = "sandbox.image"
        const val ATTR_RESULT = "result"
        const val ATTR_DROP_REASON = "drop.reason"

        private const val INSTRUMENTATION_NAME = "com.alibaba.opensandbox.sandbox"

        fun from(connectionConfig: ConnectionConfig): PoolTracer {
            if (!connectionConfig.enableTracing) return PoolTracer(null)
            return PoolTracer(
                GlobalOpenTelemetry.get().tracerBuilder(INSTRUMENTATION_NAME).build(),
            )
        }
    }
}

/**
 * One in-flight warmup trace: the root [Span] plus the ability to run work in
 * its context (with `trace_id` / `span_id` published to MDC) and to end the
 * trace with outcome attributes.
 */
internal class WarmupTrace internal constructor(
    private val root: Span,
) {
    val traceId: String
        get() = root.spanContext.traceId

    val spanId: String
        get() = root.spanContext.spanId

    /**
     * Runs [block] with this trace's root span current (child spans
     * auto-parent to it) and `trace_id`/`span_id` in the SLF4J MDC for the
     * duration of [block]. The previous thread-local MDC values are restored
     * afterwards. Never throws.
     */
    fun <T> withCurrent(block: () -> T): T {
        val prevTrace = safeMdcGet(PoolTracer.MDC_TRACE_ID)
        val prevSpan = safeMdcGet(PoolTracer.MDC_SPAN_ID)
        safeMdcPut(PoolTracer.MDC_TRACE_ID, traceId)
        safeMdcPut(PoolTracer.MDC_SPAN_ID, spanId)
        val scope = root.makeCurrent()
        return try {
            block()
        } finally {
            safeClose(scope)
            safeMdcRestore(PoolTracer.MDC_TRACE_ID, prevTrace)
            safeMdcRestore(PoolTracer.MDC_SPAN_ID, prevSpan)
        }
    }

    /** Ends the trace as successful, recording sandbox identity for drill-down. */
    fun endSuccess(
        sandboxId: String,
        image: String?,
    ) {
        root.setAttribute(PoolTracer.ATTR_SANDBOX_ID, sandboxId)
        if (!image.isNullOrBlank()) {
            root.setAttribute(PoolTracer.ATTR_SANDBOX_IMAGE, image)
        }
        root.setAttribute(PoolTracer.ATTR_RESULT, RESULT_SUCCESS)
        root.end()
    }

    /** Ends the trace as failed, recording the failure. */
    fun endFailure(error: Throwable) {
        root.recordException(error)
        root.setAttribute(PoolTracer.ATTR_RESULT, RESULT_FAILURE)
        root.end()
    }

    /**
     * Ends the trace as failed because the warmup outcome could not be
     * committed (stale run, primary lock lost, or putIdle failure) — the
     * sandbox never entered the idle pool and is scheduled for cleanup.
     * [source] matches the pool's cleanup-source values, e.g.
     * `warmup-lock-lost`.
     */
    fun endDropped(source: String) {
        root.setAttribute(PoolTracer.ATTR_RESULT, RESULT_FAILURE)
        root.setAttribute(PoolTracer.ATTR_DROP_REASON, source)
        root.end()
    }

    private companion object {
        const val RESULT_SUCCESS = "success"
        const val RESULT_FAILURE = "failure"
    }
}

private fun safeMdcGet(key: String): String? =
    try {
        MDC.get(key)
    } catch (_: Throwable) {
        null
    }

private fun safeMdcPut(
    key: String,
    value: String,
) {
    try {
        MDC.put(key, value)
    } catch (_: Throwable) {
        // best-effort
    }
}

private fun safeMdcRestore(
    key: String,
    previous: String?,
) {
    try {
        if (previous == null) {
            MDC.remove(key)
        } else {
            MDC.put(key, previous)
        }
    } catch (_: Throwable) {
        // best-effort
    }
}

private fun safeClose(scope: io.opentelemetry.context.Scope) {
    try {
        scope.close()
    } catch (_: Throwable) {
        // best-effort
    }
}
