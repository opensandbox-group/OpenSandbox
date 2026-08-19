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
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.time.Duration

class RetryPolicyTest {
    @Test
    fun `default policy retries idempotent methods on the narrow transient set`() {
        val policy = RetryPolicy()
        assertEquals(3, policy.maxRetries)
        assertEquals(
            setOf(StatusCode.TOO_MANY_REQUESTS, StatusCode.BAD_GATEWAY, StatusCode.SERVICE_UNAVAILABLE),
            policy.retryableStatusesFor("GET"),
        )
        assertTrue(policy.retryableStatusesFor("POST").isEmpty())
        assertTrue(policy.wrapsTransport())
    }

    @Test
    fun `disabled policy does not wrap the transport`() {
        val policy = RetryPolicy.disabled()
        assertEquals(0, policy.maxRetries)
        assertFalse(policy.wrapsTransport())
    }

    @Test
    fun `disabled policy still wraps when timeout knobs are set`() {
        val policy = RetryPolicy(maxRetries = 0, overallDeadline = Duration.ofSeconds(5))
        assertTrue(policy.wrapsTransport())
    }

    @Test
    fun `non-idempotent opt-in surfaces for POST`() {
        val policy =
            RetryPolicy(
                retryableStatusCodesNonIdempotent =
                    setOf(
                        StatusCode.TOO_MANY_REQUESTS,
                        StatusCode.BAD_GATEWAY,
                    ),
            )
        assertEquals(
            setOf(StatusCode.TOO_MANY_REQUESTS, StatusCode.BAD_GATEWAY),
            policy.retryableStatusesFor("POST"),
        )
        assertEquals(
            setOf(StatusCode.TOO_MANY_REQUESTS, StatusCode.BAD_GATEWAY),
            policy.retryableStatusesFor("PATCH"),
        )
    }

    @Test
    fun `status sets are defensive immutable copies`() {
        val idempotentStatuses = mutableSetOf(StatusCode.TOO_MANY_REQUESTS)
        val nonIdempotentStatuses = mutableSetOf(StatusCode.BAD_GATEWAY)
        val policy =
            RetryPolicy(
                retryableStatusCodesIdempotent = idempotentStatuses,
                retryableStatusCodesNonIdempotent = nonIdempotentStatuses,
            )

        idempotentStatuses.add(StatusCode.SERVICE_UNAVAILABLE)
        nonIdempotentStatuses.add(StatusCode.SERVICE_UNAVAILABLE)

        assertEquals(setOf(StatusCode.TOO_MANY_REQUESTS), policy.retryableStatusCodesIdempotent)
        assertEquals(setOf(StatusCode.BAD_GATEWAY), policy.retryableStatusCodesNonIdempotent)
        assertThrows(UnsupportedOperationException::class.java) {
            (policy.retryableStatusCodesIdempotent as MutableSet<Int>).add(StatusCode.SERVICE_UNAVAILABLE)
        }
        assertThrows(UnsupportedOperationException::class.java) {
            (policy.retryableStatusCodesNonIdempotent as MutableSet<Int>).add(StatusCode.SERVICE_UNAVAILABLE)
        }
        assertThrows(UnsupportedOperationException::class.java) {
            (RetryPolicy.DEFAULT_IDEMPOTENT_STATUS as MutableSet<Int>).add(StatusCode.GATEWAY_TIMEOUT)
        }
    }

    @Test
    fun `validation rejects invalid values`() {
        assertThrows(IllegalArgumentException::class.java) { RetryPolicy(maxRetries = -1) }
        assertThrows(IllegalArgumentException::class.java) { RetryPolicy(backoffMultiplier = 0.5) }
        assertThrows(IllegalArgumentException::class.java) {
            RetryPolicy(perAttemptTimeout = Duration.ZERO)
        }
        assertThrows(IllegalArgumentException::class.java) {
            RetryPolicy(overallDeadline = Duration.ofSeconds(-1))
        }
    }

    @Test
    fun `retry cause maps known statuses`() {
        assertEquals(RetryCause.STATUS_429, RetryCause.forStatus(429))
        assertEquals(RetryCause.STATUS_502, RetryCause.forStatus(502))
        assertEquals(RetryCause.STATUS_OTHER, RetryCause.forStatus(418))
    }
}
