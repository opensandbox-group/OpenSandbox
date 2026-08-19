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

import java.time.Duration
import java.util.Collections

private fun immutableStatusSetCopy(statuses: Set<Int>): Set<Int> = Collections.unmodifiableSet(LinkedHashSet(statuses))

/** RFC 9110 §9.2.2 idempotent methods. */
private val IDEMPOTENT_METHODS: Set<String> = setOf("GET", "HEAD", "PUT", "DELETE", "OPTIONS")

internal fun isIdempotentMethod(method: String): Boolean = method.uppercase() in IDEMPOTENT_METHODS

/** Backoff jitter algorithm. */
enum class JitterMode {
    NONE,
    FULL,
    DECORRELATED,
}

/** Failure classification carried on [RetryEvent]. */
enum class RetryCause(val value: String) {
    PRE_SEND("pre_send"),
    OPAQUE_TRANSPORT("opaque_transport"),
    READ_TIMEOUT("read_timeout"),
    WRITE_TIMEOUT("write_timeout"),
    UNEXPECTED_EOF("unexpected_eof"),
    STATUS_408("status_408"),
    STATUS_425("status_425"),
    STATUS_429("status_429"),
    STATUS_500("status_500"),
    STATUS_502("status_502"),
    STATUS_503("status_503"),
    STATUS_504("status_504"),
    STATUS_OTHER("status_other"),
    ;

    companion object {
        fun forStatus(statusCode: Int): RetryCause =
            when (statusCode) {
                408 -> STATUS_408
                425 -> STATUS_425
                429 -> STATUS_429
                500 -> STATUS_500
                502 -> STATUS_502
                503 -> STATUS_503
                504 -> STATUS_504
                else -> STATUS_OTHER
            }
    }
}

/**
 * Payload passed to [RetryPolicy.onRetry] before each backoff sleep.
 *
 * @property attempt 1-based index of the upcoming attempt (retry #1 is `attempt=2`).
 * @property retriesUsed retries consumed so far, i.e. `attempt - 1`.
 * @property requestId `X-Request-ID` header from the last response, if present.
 */
data class RetryEvent(
    val attempt: Int,
    val retriesUsed: Int,
    val method: String,
    val url: String,
    val cause: RetryCause,
    val statusCode: Int?,
    val backoff: Duration,
    val requestId: String?,
    val exception: Throwable?,
)

/**
 * Retry configuration for non-streaming requests.
 *
 * Defaults retry idempotent methods only. Use [disabled] to disable SDK-policy
 * retries and fall back to OkHttp's built-in connection recovery. Extend
 * [retryableStatusCodesNonIdempotent] to opt `POST`/`PATCH` in on specific status
 * codes; the SDK never does this on the caller's behalf.
 *
 * This is a value type; adding fields in future is backward-compatible as long as
 * new fields default to current behavior.
 */
class RetryPolicy
    @JvmOverloads
    constructor(
        /** Retries after the initial attempt. `0` disables SDK-policy retries. */
        val maxRetries: Int = DEFAULT_MAX_RETRIES,
        val initialBackoff: Duration = Duration.ofMillis(500),
        val maxBackoff: Duration = Duration.ofSeconds(30),
        val backoffMultiplier: Double = 2.0,
        val jitter: JitterMode = JitterMode.DECORRELATED,
        retryableStatusCodesIdempotent: Set<Int> = DEFAULT_IDEMPOTENT_STATUS,
        retryableStatusCodesNonIdempotent: Set<Int> = emptySet(),
        val perAttemptTimeout: Duration? = null,
        /** Wall-clock cap across all attempts of one logical request. */
        val overallDeadline: Duration? = null,
        /** Non-blocking hook fired synchronously before each backoff sleep. */
        val onRetry: ((RetryEvent) -> Unit)? = null,
    ) {
        /** Statuses that trigger retry for `GET/HEAD/PUT/DELETE/OPTIONS`. */
        val retryableStatusCodesIdempotent: Set<Int> =
            immutableStatusSetCopy(retryableStatusCodesIdempotent)

        /** Statuses that trigger retry for `POST/PATCH`. Empty by default. */
        val retryableStatusCodesNonIdempotent: Set<Int> =
            immutableStatusSetCopy(retryableStatusCodesNonIdempotent)

        init {
            require(maxRetries >= 0) { "maxRetries must be >= 0, got $maxRetries" }
            require(!initialBackoff.isNegative) { "initialBackoff must be >= 0, got $initialBackoff" }
            require(!maxBackoff.isNegative) { "maxBackoff must be >= 0, got $maxBackoff" }
            require(backoffMultiplier >= 1.0) { "backoffMultiplier must be >= 1.0, got $backoffMultiplier" }
            require(perAttemptTimeout == null || (!perAttemptTimeout.isNegative && !perAttemptTimeout.isZero)) {
                "perAttemptTimeout must be > 0 when set, got $perAttemptTimeout"
            }
            require(overallDeadline == null || (!overallDeadline.isNegative && !overallDeadline.isZero)) {
                "overallDeadline must be > 0 when set, got $overallDeadline"
            }
        }

        /**
         * Whether this policy requires the retry interceptor to be installed.
         *
         * Even `maxRetries == 0` needs the interceptor when the caller uses it
         * purely to enforce [perAttemptTimeout] or [overallDeadline] on a single
         * attempt, or to observe failures through [onRetry].
         */
        fun wrapsTransport(): Boolean =
            maxRetries > 0 ||
                perAttemptTimeout != null ||
                overallDeadline != null ||
                onRetry != null

        internal fun retryableStatusesFor(method: String): Set<Int> =
            if (isIdempotentMethod(method)) {
                retryableStatusCodesIdempotent
            } else {
                retryableStatusCodesNonIdempotent
            }

        companion object {
            const val DEFAULT_MAX_RETRIES = 3

            /**
             * Default idempotent retryable set: 429 (Too Many Requests), 502
             * (Bad Gateway), 503 (Service Unavailable). Narrower than the OSEP
             * draft's `{408, 425, 429, 500, 502, 503, 504}`, matching the Python
             * landing.
             */
            @JvmField
            val DEFAULT_IDEMPOTENT_STATUS: Set<Int> =
                immutableStatusSetCopy(
                    setOf(
                        StatusCode.TOO_MANY_REQUESTS,
                        StatusCode.BAD_GATEWAY,
                        StatusCode.SERVICE_UNAVAILABLE,
                    ),
                )

            /** Disable SDK-policy retries and fall back to OkHttp's built-in connection recovery. */
            @JvmStatic
            fun disabled(): RetryPolicy = RetryPolicy(maxRetries = 0)
        }
    }

/**
 * Well-known HTTP status codes used by [RetryPolicy].
 *
 * Provided so callers can compose status sets with named constants instead of
 * bare integers, matching the Python SDK's use of `HTTPStatus`.
 */
object StatusCode {
    const val REQUEST_TIMEOUT = 408
    const val TOO_EARLY = 425
    const val TOO_MANY_REQUESTS = 429
    const val INTERNAL_SERVER_ERROR = 500
    const val BAD_GATEWAY = 502
    const val SERVICE_UNAVAILABLE = 503
    const val GATEWAY_TIMEOUT = 504
}
