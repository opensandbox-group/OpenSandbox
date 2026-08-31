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
import com.alibaba.opensandbox.sandbox.pool.WarmupResult
import com.alibaba.opensandbox.sandbox.pool.WarmupStage
import com.alibaba.opensandbox.sandbox.pool.WarmupTerminalOutcome
import com.alibaba.opensandbox.sandbox.pool.classifyWarmupError
import io.opentelemetry.api.GlobalOpenTelemetry
import io.opentelemetry.api.trace.Span
import io.opentelemetry.api.trace.StatusCode
import io.opentelemetry.api.trace.Tracer
import io.opentelemetry.context.Context
import org.slf4j.MDC
import java.util.concurrent.TimeUnit

/**
 * Best-effort OpenTelemetry tracing for the client-side pool warmup path.
 *
 * Tracing is opt-in via [ConnectionConfig.enableTracing]. When enabled, each
 * warmup task produces one trace rooted at a [WARMUP_ROOT_SPAN] span with
 * per-phase child spans (create / readiness / prepare / post-prepare check /
 * renew / commit). The root span starts at task submission time so
 * queue-waiting time is visible as the gap before the first child span.
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
        leaderEpoch: Long,
        submittedEpochNanos: Long,
    ): WarmupTrace? {
        val t = tracer ?: return null
        return safeTelemetry {
            val root =
                t.spanBuilder(WARMUP_ROOT_SPAN)
                    .setAttribute(ATTR_POOL_NAME, poolName)
                    .setAttribute(ATTR_POOL_OWNER, ownerId)
                    .setAttribute(ATTR_POOL_RUN_GENERATION, runGeneration)
                    .setAttribute(ATTR_POOL_LEADER_EPOCH, leaderEpoch)
                    .setStartTimestamp(submittedEpochNanos, TimeUnit.NANOSECONDS)
                    .startSpan()
            WarmupTrace(t, root)
        }
    }

    /**
     * Runs [block] under a child span of the currently-current span (the
     * warmup root). No-op span when tracing is disabled.
     */
    internal fun <T> withPhaseSpan(
        spanName: String,
        block: () -> T,
    ): T = withOutcomePhaseSpan(spanName, { null }, block)

    /**
     * Variant for phases such as commit that return a domain failure instead
     * of throwing. A non-null [outcome] marks the child span consistently
     * with the terminal warmup result.
     */
    internal fun <T> withOutcomePhaseSpan(
        spanName: String,
        outcome: (T) -> WarmupTerminalOutcome?,
        block: () -> T,
    ): T {
        val t = tracer ?: return block()
        val span = safeTelemetry { t.spanBuilder(spanName).startSpan() } ?: return block()
        val scope = safeTelemetry { span.makeCurrent() }
        return try {
            block().also { value ->
                val phaseOutcome = safeTelemetry { outcome(value) }
                if (phaseOutcome == null) {
                    safeTelemetry { span.setAttribute(ATTR_RESULT, WarmupResult.SUCCESS.value) }
                } else {
                    annotateOutcome(span, phaseOutcome)
                }
            }
        } catch (failure: Throwable) {
            safeTelemetry {
                span.setAttribute(ATTR_RESULT, WarmupResult.FAILURE.value)
                span.setAttribute(
                    ATTR_ERROR_CATEGORY,
                    classifyWarmupError(stageForSpan(spanName), failure)?.value ?: "unclassified",
                )
                span.setAttribute(ATTR_ERROR_TYPE, failure.javaClass.name)
                span.setStatus(StatusCode.ERROR)
                span.recordException(failure)
            }
            throw failure
        } finally {
            scope?.let(::safeClose)
            safeTelemetry { span.end() }
        }
    }

    private fun annotateOutcome(
        span: Span,
        outcome: WarmupTerminalOutcome,
    ) {
        safeTelemetry {
            span.setAttribute(ATTR_STAGE, outcome.stage.value)
            span.setAttribute(ATTR_RESULT, outcome.result.value)
            outcome.reason?.let { span.setAttribute(ATTR_REASON, it.value) }
            outcome.errorCategory?.let { span.setAttribute(ATTR_ERROR_CATEGORY, it.value) }
            outcome.error?.let { span.setAttribute(ATTR_ERROR_TYPE, it.javaClass.name) }
            if (outcome.result == WarmupResult.FAILURE || outcome.result == WarmupResult.DROPPED) {
                span.setStatus(StatusCode.ERROR)
            }
            outcome.error?.let(span::recordException)
        }
    }

    private fun stageForSpan(spanName: String): WarmupStage =
        when (spanName) {
            WARMUP_CREATE_SPAN -> WarmupStage.CREATE
            WARMUP_READINESS_CHECK_SPAN -> WarmupStage.READINESS
            WARMUP_PREPARE_SPAN -> WarmupStage.PREPARE
            WARMUP_POST_PREPARE_CHECK_SPAN -> WarmupStage.POST_PREPARE_READINESS
            WARMUP_RENEW_SPAN -> WarmupStage.RENEW
            WARMUP_COMMIT_SPAN -> WarmupStage.COMMIT
            else -> WarmupStage.ADMISSION
        }

    companion object {
        const val WARMUP_ROOT_SPAN = "pool.warmup"
        const val WARMUP_CREATE_SPAN = "pool.warmup.create"
        const val WARMUP_READINESS_CHECK_SPAN = "pool.warmup.readiness"
        const val WARMUP_PREPARE_SPAN = "pool.warmup.prepare"
        const val WARMUP_POST_PREPARE_CHECK_SPAN = "pool.warmup.post_prepare_readiness"
        const val WARMUP_RENEW_SPAN = "pool.warmup.renew"
        const val WARMUP_COMMIT_SPAN = "pool.warmup.commit"

        const val MDC_TRACE_ID = "trace_id"
        const val MDC_SPAN_ID = "span_id"

        const val ATTR_POOL_NAME = "pool.name"
        const val ATTR_POOL_OWNER = "pool.owner"
        const val ATTR_POOL_RUN_GENERATION = "pool.run.generation"
        const val ATTR_POOL_LEADER_EPOCH = "pool.leader.epoch"
        const val ATTR_SANDBOX_ID = "sandbox.id"
        const val ATTR_SANDBOX_IMAGE = "sandbox.image"
        const val ATTR_STAGE = "warmup.stage"
        const val ATTR_RESULT = "warmup.result"
        const val ATTR_REASON = "warmup.reason"
        const val ATTR_ERROR_CATEGORY = "warmup.error.category"
        const val ATTR_ERROR_TYPE = "warmup.error.type"
        const val ATTR_HEALTH_ATTEMPT_COUNT = "warmup.health.attempt_count"
        const val ATTR_HEALTH_FALSE_COUNT = "warmup.health.false_count"
        const val ATTR_HEALTH_EXCEPTION_COUNT = "warmup.health.exception_count"
        const val ATTR_SCHEDULER_DELAY_MS = "warmup.scheduler.delay_ms"

        @Deprecated("Use ATTR_REASON")
        const val ATTR_DROP_REASON = ATTR_REASON

        private const val INSTRUMENTATION_NAME = "com.alibaba.opensandbox.sandbox"

        fun from(connectionConfig: ConnectionConfig): PoolTracer {
            if (!connectionConfig.enableTracing) return PoolTracer(null)
            return PoolTracer(
                safeTelemetry {
                    GlobalOpenTelemetry.get().tracerBuilder(INSTRUMENTATION_NAME).build()
                },
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
    private val tracer: Tracer,
    private val root: Span,
) {
    val traceId: String
        get() = safeTelemetry { root.spanContext.traceId }.orEmpty()

    val spanId: String
        get() = safeTelemetry { root.spanContext.spanId }.orEmpty()

    /**
     * Runs [block] with this trace's root span current (child spans
     * auto-parent to it) and `trace_id`/`span_id` in the SLF4J MDC for the
     * duration of [block]. The previous thread-local MDC values are restored
     * afterwards. Never throws.
     */
    fun <T> withCurrent(block: () -> T): T {
        val prevTrace = safeMdcGet(PoolTracer.MDC_TRACE_ID)
        val prevSpan = safeMdcGet(PoolTracer.MDC_SPAN_ID)
        traceId.takeIf(String::isNotBlank)?.let { safeMdcPut(PoolTracer.MDC_TRACE_ID, it) }
        spanId.takeIf(String::isNotBlank)?.let { safeMdcPut(PoolTracer.MDC_SPAN_ID, it) }
        val scope = safeTelemetry { root.makeCurrent() }
        return try {
            block()
        } finally {
            scope?.let(::safeClose)
            safeMdcRestore(PoolTracer.MDC_TRACE_ID, prevTrace)
            safeMdcRestore(PoolTracer.MDC_SPAN_ID, prevSpan)
        }
    }

    /** Emits one backdated summary span for an entire health-check polling stage. */
    fun endHealthStage(
        spanName: String,
        stage: WarmupStage,
        startEpochNanos: Long,
        endEpochNanos: Long,
        attemptCount: Long,
        falseCount: Long,
        exceptionCount: Long,
        schedulerDelayNanos: Long,
        result: WarmupResult,
        error: Throwable? = null,
    ) {
        val span =
            safeTelemetry {
                tracer.spanBuilder(spanName)
                    .setParent(Context.root().with(root))
                    .setStartTimestamp(startEpochNanos, TimeUnit.NANOSECONDS)
                    .startSpan()
            } ?: return
        try {
            safeTelemetry {
                span.setAttribute(PoolTracer.ATTR_STAGE, stage.value)
                span.setAttribute(PoolTracer.ATTR_RESULT, result.value)
                span.setAttribute(PoolTracer.ATTR_HEALTH_ATTEMPT_COUNT, attemptCount)
                span.setAttribute(PoolTracer.ATTR_HEALTH_FALSE_COUNT, falseCount)
                span.setAttribute(PoolTracer.ATTR_HEALTH_EXCEPTION_COUNT, exceptionCount)
                span.setAttribute(
                    PoolTracer.ATTR_SCHEDULER_DELAY_MS,
                    schedulerDelayNanos.coerceAtLeast(0L).toDouble() / 1_000_000.0,
                )
                error?.let {
                    classifyWarmupError(stage, it)?.let { category ->
                        span.setAttribute(PoolTracer.ATTR_ERROR_CATEGORY, category.value)
                    }
                    span.setAttribute(PoolTracer.ATTR_ERROR_TYPE, it.javaClass.name)
                }
                if (result == WarmupResult.FAILURE || result == WarmupResult.DROPPED) {
                    span.setStatus(StatusCode.ERROR)
                }
                error?.let(span::recordException)
            }
        } finally {
            safeTelemetry { span.end(endEpochNanos, TimeUnit.NANOSECONDS) }
        }
    }

    /** Ends the trace once with the task's unified terminal outcome. */
    fun end(
        outcome: WarmupTerminalOutcome,
        image: String?,
    ) {
        try {
            safeTelemetry {
                outcome.sandboxId?.let { root.setAttribute(PoolTracer.ATTR_SANDBOX_ID, it) }
                if (!image.isNullOrBlank()) root.setAttribute(PoolTracer.ATTR_SANDBOX_IMAGE, image)
                root.setAttribute(PoolTracer.ATTR_STAGE, outcome.stage.value)
                root.setAttribute(PoolTracer.ATTR_RESULT, outcome.result.value)
                outcome.reason?.let { root.setAttribute(PoolTracer.ATTR_REASON, it.value) }
                outcome.errorCategory?.let { root.setAttribute(PoolTracer.ATTR_ERROR_CATEGORY, it.value) }
                outcome.error?.let { root.setAttribute(PoolTracer.ATTR_ERROR_TYPE, it.javaClass.name) }
                if (outcome.result == WarmupResult.FAILURE || outcome.result == WarmupResult.DROPPED) {
                    root.setStatus(StatusCode.ERROR)
                }
            }
        } finally {
            safeTelemetry { root.end() }
        }
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

private inline fun <T> safeTelemetry(block: () -> T): T? =
    try {
        block()
    } catch (_: Throwable) {
        null
    }
