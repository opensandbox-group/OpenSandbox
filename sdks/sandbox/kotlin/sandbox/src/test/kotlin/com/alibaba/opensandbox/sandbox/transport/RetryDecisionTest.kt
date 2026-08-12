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

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.time.Duration
import java.time.ZoneOffset
import java.time.ZonedDateTime
import java.util.Random

class RetryDecisionTest {
    private val defaultPolicy = RetryPolicy()

    private fun statusOutcome(code: Int) = Outcome(isTransportError = false, statusCode = code, cause = RetryCause.forStatus(code))

    private val preSend =
        Outcome(isTransportError = true, isPreSend = true, cause = RetryCause.PRE_SEND)
    private val readTimeout =
        Outcome(isTransportError = true, cause = RetryCause.READ_TIMEOUT)

    @Test
    fun `idempotent retries on transient status`() {
        for (code in listOf(429, 502, 503)) {
            assertTrue(
                shouldRetry("GET", statusOutcome(code), 0, defaultPolicy, Duration.ZERO),
                "GET should retry on $code",
            )
        }
    }

    @Test
    fun `POST does not retry on any status under default policy`() {
        for (code in listOf(429, 500, 502, 503, 504)) {
            assertFalse(
                shouldRetry("POST", statusOutcome(code), 0, defaultPolicy, Duration.ZERO),
                "POST should not retry on $code",
            )
        }
    }

    @Test
    fun `4xx not in set is never retried`() {
        for (code in listOf(400, 401, 404, 409)) {
            assertFalse(shouldRetry("GET", statusOutcome(code), 0, defaultPolicy, Duration.ZERO))
            assertFalse(shouldRetry("POST", statusOutcome(code), 0, defaultPolicy, Duration.ZERO))
        }
    }

    @Test
    fun `pre-send retries on any method`() {
        assertTrue(shouldRetry("GET", preSend, 0, defaultPolicy, Duration.ZERO))
        assertTrue(shouldRetry("POST", preSend, 0, defaultPolicy, Duration.ZERO))
    }

    @Test
    fun `read timeout retries idempotent only`() {
        assertTrue(shouldRetry("GET", readTimeout, 0, defaultPolicy, Duration.ZERO))
        assertFalse(shouldRetry("POST", readTimeout, 0, defaultPolicy, Duration.ZERO))
    }

    @Test
    fun `budget exhaustion stops retry`() {
        assertFalse(shouldRetry("GET", statusOutcome(503), 3, defaultPolicy, Duration.ZERO))
    }

    @Test
    fun `overall deadline stops retry`() {
        val policy = RetryPolicy(overallDeadline = Duration.ofSeconds(10))
        assertFalse(
            shouldRetry("GET", statusOutcome(503), 0, policy, Duration.ofSeconds(11)),
        )
    }

    @Test
    fun `cancellation stops retry`() {
        assertFalse(
            shouldRetry("GET", statusOutcome(503), 0, defaultPolicy, Duration.ZERO, cancelled = true),
        )
    }

    @Test
    fun `POST opt-in enables status retry`() {
        val policy = RetryPolicy(retryableStatusCodesNonIdempotent = setOf(429, 502))
        assertTrue(shouldRetry("POST", statusOutcome(429), 0, policy, Duration.ZERO))
        assertTrue(shouldRetry("POST", statusOutcome(502), 0, policy, Duration.ZERO))
        assertFalse(shouldRetry("POST", statusOutcome(503), 0, policy, Duration.ZERO))
        // Opt-in does not lift post-send read timeouts for non-idempotent methods.
        assertFalse(shouldRetry("POST", readTimeout, 0, policy, Duration.ZERO))
    }

    @Test
    fun `non-replayable body suppresses status retry and second pre-send`() {
        assertFalse(
            shouldRetry("GET", statusOutcome(503), 0, defaultPolicy, Duration.ZERO, bodyReplayable = false),
        )
        // First pre-send is still allowed (no bytes written yet).
        assertTrue(
            shouldRetry("POST", preSend, 0, defaultPolicy, Duration.ZERO, bodyReplayable = false),
        )
        // Beyond the first attempt, suppressed.
        assertFalse(
            shouldRetry("POST", preSend, 1, defaultPolicy, Duration.ZERO, bodyReplayable = false),
        )
    }

    @Test
    fun `parse retry-after delta seconds`() {
        assertEquals(Duration.ofSeconds(2), parseRetryAfter("2"))
        assertEquals(Duration.ZERO, parseRetryAfter("-5"))
        assertNull(parseRetryAfter(null))
        assertNull(parseRetryAfter(""))
        assertNull(parseRetryAfter("not-a-number-or-date"))
    }

    @Test
    fun `parse retry-after http date`() {
        val now = ZonedDateTime.of(2026, 1, 1, 0, 0, 0, 0, ZoneOffset.UTC)
        val future = "Thu, 01 Jan 2026 00:00:30 GMT"
        assertEquals(Duration.ofSeconds(30), parseRetryAfter(future, now))
        val past = "Wed, 01 Jan 2020 00:00:00 GMT"
        assertEquals(Duration.ZERO, parseRetryAfter(past, now))
    }

    @Test
    fun `retry-after cap clamps to 60s`() {
        assertEquals(Duration.ofSeconds(60), applyRetryAfterCap(Duration.ofSeconds(3600)))
        assertEquals(Duration.ofSeconds(5), applyRetryAfterCap(Duration.ofSeconds(5)))
        assertNull(applyRetryAfterCap(null))
    }

    @Test
    fun `compute backoff none jitter is deterministic exponential`() {
        val policy =
            RetryPolicy(
                jitter = JitterMode.NONE,
                initialBackoff = Duration.ofMillis(100),
                maxBackoff = Duration.ofSeconds(30),
            )
        val rng = Random(0)
        assertEquals(100, computeBackoff(0, policy, Duration.ZERO, rng).toMillis())
        assertEquals(200, computeBackoff(1, policy, Duration.ZERO, rng).toMillis())
        assertEquals(400, computeBackoff(2, policy, Duration.ZERO, rng).toMillis())
    }

    @Test
    fun `compute backoff respects max backoff on first decorrelated retry`() {
        val policy =
            RetryPolicy(
                jitter = JitterMode.DECORRELATED,
                initialBackoff = Duration.ofSeconds(10),
                maxBackoff = Duration.ofSeconds(1),
            )
        val delay = computeBackoff(0, policy, Duration.ZERO, Random(0))
        assertTrue(delay <= Duration.ofSeconds(1))
    }
}
