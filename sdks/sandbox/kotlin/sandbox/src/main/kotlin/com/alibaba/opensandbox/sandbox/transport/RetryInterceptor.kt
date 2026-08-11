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

package com.alibaba.opensandbox.sandbox.transport

import okhttp3.Interceptor
import okhttp3.Response
import org.slf4j.LoggerFactory
import java.io.IOException
import java.io.InterruptedIOException
import java.net.ConnectException
import java.net.NoRouteToHostException
import java.net.SocketTimeoutException
import java.net.UnknownHostException
import java.time.Duration
import java.util.Random
import java.util.concurrent.TimeUnit
import javax.net.ssl.SSLException
import javax.net.ssl.SSLHandshakeException

/**
 * OkHttp application interceptor enforcing a [RetryPolicy].
 *
 * Installed on the SDK's non-streaming clients only. SSE clients are deliberately
 * excluded because they hold the response body open and their request bodies are
 * not safely replayable for a non-idempotent status opt-in.
 *
 * OkHttp `RequestBody` is re-readable by default (`isOneShot() == false`), so
 * replay is safe. Streaming bodies with unknown content length
 * (`contentLength() == -1`, e.g.  InputStream wrappers from the filesystem
 * adapters) are treated as non-replayable to avoid truncation.
 */
class RetryInterceptor
    @JvmOverloads
    constructor(
        private val policy: RetryPolicy,
        private val rng: Random = Random(),
        private val sleeper: (Duration) -> Unit = ::defaultSleeper,
    ) : Interceptor {
        private val logger = LoggerFactory.getLogger(RetryInterceptor::class.java)

        override fun intercept(chain: Interceptor.Chain): Response {
            val request = chain.request()
            val method = request.method.uppercase()
            // OkHttp RequestBody is re-readable by default (isOneShot() ==
            // false), but the SDK wraps InputStream uploads in anonymous
            // RequestBody subclasses that do not override isOneShot() even
            // though their stream can only be consumed once.
            // contentLength() == -1 signals an unknown-length streaming body
            // — treat it as non-replayable for status-code retries.
            val bodyReplayable =
                request.body?.isOneShot() != true &&
                    request.body?.contentLength() != -1L

            val start = System.nanoTime()
            var previousSleep: Duration = Duration.ZERO
            var retriesUsed = 0
            var lastException: IOException? = null

            while (true) {
                // Surface caller cancellation immediately — do not retry.
                if (chain.call().isCanceled()) {
                    lastException?.let { throw it }
                    throw IOException("call cancelled")
                }
                val elapsedBefore = Duration.ofNanos(System.nanoTime() - start)
                // Bail out before dispatching if the wall-clock budget is already
                // gone; surface a timeout mapped to SandboxTimeoutException.
                policy.overallDeadline?.let { deadline ->
                    val remaining = deadline.minus(elapsedBefore)
                    if (remaining <= Duration.ZERO) {
                        throw RetryDeadlineExceededException(
                            "retry overallDeadline exceeded before next attempt",
                            lastException,
                        )
                    }
                }

                // Clamp per-attempt timeouts: apply perAttemptTimeout and remaining
                // overallDeadline, whichever is tighter. OkHttp chain.with*Timeout
                // returns a new Chain with the timeout lowered (not raised), so
                // default values from the caller's OkHttpClient are preserved when
                // these knobs are unset.
                var effectiveChain = chain
                val perAttemptMs = computePerAttemptTimeoutMs(elapsedBefore)
                if (perAttemptMs != null) {
                    val ms = perAttemptMs.toInt().coerceAtLeast(1)
                    effectiveChain = effectiveChain.withConnectTimeout(ms, TimeUnit.MILLISECONDS)
                    effectiveChain = effectiveChain.withReadTimeout(ms, TimeUnit.MILLISECONDS)
                    effectiveChain = effectiveChain.withWriteTimeout(ms, TimeUnit.MILLISECONDS)
                }

                var response: Response? = null
                var exception: IOException? = null
                val outcome: Outcome =
                    try {
                        response = effectiveChain.proceed(request)
                        outcomeForResponse(response.code)
                    } catch (e: InterruptedIOException) {
                        // A caller interrupt is cancellation, not a retryable
                        // timeout. Preserve the flag and surface the original
                        // exception before the retry decision can schedule work.
                        if (Thread.currentThread().isInterrupted) {
                            throw e
                        }
                        exception = e
                        classifyIoException(e)
                    } catch (e: IOException) {
                        exception = e
                        classifyIoException(e)
                    }

                val elapsedAfter = Duration.ofNanos(System.nanoTime() - start)
                val cancelled = chain.call().isCanceled()
                val retry =
                    shouldRetry(
                        method = method,
                        outcome = outcome,
                        retriesUsed = retriesUsed,
                        policy = policy,
                        elapsed = elapsedAfter,
                        cancelled = cancelled,
                        bodyReplayable = bodyReplayable,
                    )

                if (!retry) {
                    if (exception != null) throw exception
                    return response!!
                }

                lastException = exception

                // Compute backoff: honor Retry-After when present.
                var retryAfter: Duration? = null
                if (response != null) {
                    retryAfter = applyRetryAfterCap(parseRetryAfter(response.header("Retry-After")))
                }
                var sleepFor =
                    retryAfter
                        ?: computeBackoff(retriesUsed, policy, previousSleep, rng)

                // Clamp the sleep to the remaining overall deadline so a long
                // Retry-After or backoff cannot push us past the caller budget.
                policy.overallDeadline?.let { deadline ->
                    val remaining = deadline.minus(elapsedAfter)
                    if (sleepFor > remaining) {
                        sleepFor = if (remaining.isNegative) Duration.ZERO else remaining
                    }
                }

                // Do not sleep when the call has been cancelled: this avoids a
                // pointless wait when the caller already requested termination.
                if (chain.call().isCanceled()) {
                    lastException?.let { throw it }
                    throw IOException("call cancelled")
                }

                val requestId = response?.header("X-Request-ID")
                val statusCode = response?.code
                val event =
                    RetryEvent(
                        attempt = retriesUsed + 2,
                        retriesUsed = retriesUsed,
                        method = method,
                        url = request.url.toString(),
                        cause = outcome.cause,
                        statusCode = statusCode,
                        backoff = sleepFor,
                        requestId = requestId,
                        exception = exception,
                    )
                logger.warn(
                    "retrying {} {}: attempt={} cause={} status={} backoff={}ms request_id={}",
                    event.method,
                    event.url,
                    event.attempt,
                    event.cause.value,
                    event.statusCode,
                    event.backoff.toMillis(),
                    event.requestId,
                )
                policy.onRetry?.let { hook ->
                    try {
                        hook(event)
                    } catch (e: Exception) {
                        logger.warn("RetryPolicy.onRetry callback raised", e)
                    }
                }

                // Release the retried response body so the connection returns to
                // the pool before the next attempt.
                response?.close()

                retriesUsed += 1
                previousSleep = sleepFor
                sleeper(sleepFor)
                // If a thread interruption woke the sleeper, propagate it without
                // clearing the flag so callers can continue to observe cancellation.
                // The next loop iteration's isCanceled() check handles OkHttp
                // cancellations; this covers JVM-level thread interruption.
                if (Thread.currentThread().isInterrupted) {
                    lastException?.let { throw it }
                    throw IOException("call interrupted during backoff")
                }
            }
        }

        private fun outcomeForResponse(code: Int): Outcome =
            Outcome(
                isTransportError = false,
                statusCode = code,
                cause = RetryCause.forStatus(code),
            )

        /**
         * Bound the current attempt by [RetryPolicy.perAttemptTimeout] and the
         * remaining [RetryPolicy.overallDeadline], whichever is tighter.
         *
         * Returns the tightest per-phase timeout in milliseconds for this attempt,
         * or `null` when neither knob is set and default OkHttp timeouts should be
         * used.
         */
        private fun computePerAttemptTimeoutMs(elapsed: Duration): Long? {
            val limit: Duration? =
                when {
                    policy.perAttemptTimeout != null && policy.overallDeadline != null -> {
                        val remaining = policy.overallDeadline!! - elapsed
                        if (remaining <= Duration.ZERO) {
                            Duration.ZERO
                        } else {
                            minOf(
                                policy.perAttemptTimeout!!,
                                remaining,
                            )
                        }
                    }
                    policy.perAttemptTimeout != null -> policy.perAttemptTimeout
                    policy.overallDeadline != null -> maxOf(policy.overallDeadline!! - elapsed, Duration.ZERO)
                    else -> null
                }
            return limit?.let {
                val ms = it.toMillis()
                if (ms <= 0) null else ms
            }
        }

        /**
         * Classify an OkHttp-thrown [IOException].
         *
         * - Connect failures (DNS, TCP connect, TLS): pre-send, safe on any method.
         * - Read timeouts: post-send, idempotent only.
         * - Other IO errors: opaque, idempotent only.
         */
        private fun classifyIoException(e: IOException): Outcome {
            // Connect timeout: OkHttp raises SocketTimeoutException whose message
            // contains "connect". Distinguish from read timeout heuristically.
            if (e is SocketTimeoutException) {
                val msg = e.message?.lowercase().orEmpty()
                return if (msg.contains("connect")) {
                    Outcome(isTransportError = true, isPreSend = true, cause = RetryCause.PRE_SEND)
                } else {
                    Outcome(isTransportError = true, cause = RetryCause.READ_TIMEOUT)
                }
            }
            if (e is ConnectException ||
                e is UnknownHostException ||
                e is NoRouteToHostException
            ) {
                return Outcome(isTransportError = true, isPreSend = true, cause = RetryCause.PRE_SEND)
            }
            // SSLHandshakeException is pre-send (handshake never completed).
            if (e is SSLHandshakeException) {
                return Outcome(isTransportError = true, isPreSend = true, cause = RetryCause.PRE_SEND)
            }
            // Generic SSL failures do not reveal whether request bytes were sent.
            // Treat them as opaque transport failures so only idempotent methods
            // are eligible for retry.
            if (e is SSLException) {
                return Outcome(
                    isTransportError = true,
                    isOpaqueTransport = true,
                    cause = RetryCause.OPAQUE_TRANSPORT,
                )
            }
            // Generic InterruptedIOException that is not a socket timeout: treat as
            // a post-send read timeout for observability.
            if (e is InterruptedIOException) {
                return Outcome(isTransportError = true, cause = RetryCause.READ_TIMEOUT)
            }
            // Any other IOException (e.g. connection reset mid-stream, unexpected
            // EOF): opaque, retry on idempotent methods only.
            return Outcome(
                isTransportError = true,
                isOpaqueTransport = true,
                cause = RetryCause.OPAQUE_TRANSPORT,
            )
        }
    }

// Injectable-for-tests default sleeper; real code blocks the calling thread.
// Uses Thread.sleep() (interruptible) so cancellation during backoff is surfaced
// via InterruptedException → isCanceled() check on the next loop iteration.
private fun defaultSleeper(duration: Duration) {
    val millis = duration.toMillis()
    if (millis > 0) {
        try {
            Thread.sleep(millis)
        } catch (_: InterruptedException) {
            Thread.currentThread().interrupt()
        }
    }
}

/**
 * Raised by [RetryInterceptor] when the overall retry deadline is exceeded before
 * the next attempt. Mapped to `SandboxTimeoutException` by the exception converter.
 */
class RetryDeadlineExceededException(
    message: String,
    cause: Throwable? = null,
) : IOException(message, cause)
