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
import java.time.ZonedDateTime
import java.time.format.DateTimeFormatter
import java.time.format.DateTimeParseException
import java.util.Random

/** Classifier-friendly view of one request/response cycle. No OkHttp dependency. */
internal data class Outcome(
    val isTransportError: Boolean,
    val statusCode: Int? = null,
    val isPreSend: Boolean = false,
    val isOpaqueTransport: Boolean = false,
    // Read/write timeout and unexpected-EOF are useful for observability but
    // treated identically by shouldRetry.
    val cause: RetryCause = RetryCause.STATUS_OTHER,
)

/**
 * Decide whether to retry.
 *
 * Checks budget/deadline/cancellation first, then the transport-vs-status branch.
 * Pre-send transport failures retry on any method; post-send and opaque transport
 * failures retry on idempotent methods only.
 *
 * [bodyReplayable] guards non-idempotent uploads: when the request body cannot be
 * re-sent (one-shot [okhttp3.RequestBody]), re-dispatching would send a truncated
 * body. A status-code retry always consumes the body first, so it is suppressed for
 * non-replayable bodies. A pre-send failure writes no bytes on the *first* attempt,
 * so it may still be retried once; beyond that, the stream may be partially consumed
 * and pre-send retries are also suppressed.
 */
internal fun shouldRetry(
    method: String,
    outcome: Outcome,
    retriesUsed: Int,
    policy: RetryPolicy,
    elapsed: Duration,
    cancelled: Boolean = false,
    bodyReplayable: Boolean = true,
): Boolean {
    if (retriesUsed >= policy.maxRetries) return false
    if (policy.overallDeadline != null && elapsed >= policy.overallDeadline) return false
    if (cancelled) return false

    val idempotent = isIdempotentMethod(method)

    if (outcome.isTransportError) {
        if (outcome.isPreSend) {
            // First attempt wrote no bytes, so a non-replayable body is still
            // intact and safe to resend once. After that, assume the stream may
            // be partially consumed and stop.
            if (!bodyReplayable && retriesUsed >= 1) return false
            return true
        }
        return idempotent
    }

    val statusCode = outcome.statusCode ?: return false
    // A status-code retry means the body was already sent (and consumed); never
    // replay a non-replayable body.
    if (!bodyReplayable) return false
    return statusCode in policy.retryableStatusesFor(method)
}

/**
 * Backoff sleep for retry [retryIndex] (0-based). [previousSleep] is used only by
 * decorrelated jitter.
 */
internal fun computeBackoff(
    retryIndex: Int,
    policy: RetryPolicy,
    previousSleep: Duration,
    rng: Random,
): Duration {
    val maxS = policy.maxBackoff.toNanos() / 1e9
    val baseS = policy.initialBackoff.toNanos() / 1e9
    val m = policy.backoffMultiplier

    val expS = if (baseS > 0) minOf(maxS, baseS * Math.pow(m, retryIndex.toDouble())) else 0.0

    val sleepS: Double =
        when (policy.jitter) {
            JitterMode.NONE -> expS
            JitterMode.FULL -> if (expS > 0) rng.nextDouble() * expS else 0.0
            JitterMode.DECORRELATED -> {
                val prevS = previousSleep.toNanos() / 1e9
                if (retryIndex == 0 || prevS <= 0) {
                    // Clamp the first delay to maxBackoff too so a low maxBackoff
                    // is respected on the very first retry, matching other modes.
                    minOf(baseS, maxS)
                } else {
                    val upper = minOf(maxS, prevS * m)
                    val lower = baseS
                    if (upper < lower) upper else lower + rng.nextDouble() * (upper - lower)
                }
            }
        }

    return Duration.ofNanos((maxOf(0.0, sleepS) * 1e9).toLong())
}

/**
 * Fixed operational ceiling for server-supplied `Retry-After` waits. Not
 * configurable: ignoring a server's back-pressure signal is never a safe default,
 * and the 60s guard is only there to defend against a pathological header.
 */
internal val RETRY_AFTER_CAP: Duration = Duration.ofSeconds(60)

// Overflow guard for numeric Retry-After values, independent of the operational cap.
private const val MAX_RETRY_AFTER_SECONDS: Long = 100L * 365 * 24 * 3600 // ~100 years

/**
 * Parse a `Retry-After` header (delta-seconds or HTTP-date). Past dates and negative
 * deltas normalize to zero; unparseable returns `null`.
 */
internal fun parseRetryAfter(
    headerValue: String?,
    now: ZonedDateTime? = null,
): Duration? {
    if (headerValue == null) return null
    val raw = headerValue.trim()
    if (raw.isEmpty()) return null

    // delta-seconds (integer)
    val seconds = raw.toLongOrNull()
    if (seconds != null) {
        if (seconds < 0) return Duration.ZERO
        return Duration.ofSeconds(minOf(seconds, MAX_RETRY_AFTER_SECONDS))
    }

    // HTTP-date (RFC 1123)
    val parsed =
        try {
            ZonedDateTime.parse(raw, DateTimeFormatter.RFC_1123_DATE_TIME)
        } catch (_: DateTimeParseException) {
            return null
        }
    val reference = now ?: ZonedDateTime.now(parsed.zone)
    val delta = Duration.between(reference, parsed)
    return if (delta.isNegative) Duration.ZERO else delta
}

/** Cap [retryAfter] at the fixed 60-second ceiling. */
internal fun applyRetryAfterCap(retryAfter: Duration?): Duration? {
    if (retryAfter == null) return null
    return if (retryAfter > RETRY_AFTER_CAP) RETRY_AFTER_CAP else retryAfter
}
