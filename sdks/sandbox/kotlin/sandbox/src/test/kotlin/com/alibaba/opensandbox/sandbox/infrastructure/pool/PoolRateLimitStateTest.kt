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

package com.alibaba.opensandbox.sandbox.infrastructure.pool

import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.pool.PoolConfig
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.time.Duration
import java.time.Instant
import java.util.concurrent.atomic.AtomicInteger

class PoolRateLimitStateTest {
    private val now: Instant = Instant.parse("2026-08-14T00:00:00Z")

    @Test
    fun `zero or non-positive retry after falls back to default delay`() {
        val state = PoolRateLimitState()

        state.recordRateLimit(retryAfter = Duration.ZERO, now = now)

        assertTrue(state.isActive(now.plusSeconds(9)))
        assertFalse(state.isActive(now.plusSeconds(10)))
    }

    @Test
    fun `missing retry after uses bounded default delay`() {
        val state = PoolRateLimitState()

        state.recordRateLimit(retryAfter = null, now = now)

        assertTrue(state.isActive(now.plusSeconds(9)))
        assertFalse(state.isActive(now.plusSeconds(10)))
    }

    @Test
    fun `retry after is capped at transport ceiling`() {
        val state = PoolRateLimitState()

        state.recordRateLimit(retryAfter = Duration.ofMinutes(5), now = now)

        assertTrue(state.isActive(now.plusSeconds(59)))
        assertFalse(state.isActive(now.plusSeconds(60)))
    }

    @Test
    fun `concurrent rate limits only extend throttle deadline`() {
        val state = PoolRateLimitState()

        state.recordRateLimit(retryAfter = Duration.ofSeconds(30), now = now)
        state.recordRateLimit(retryAfter = Duration.ofSeconds(5), now = now.plusSeconds(1))

        assertEquals(Duration.ofSeconds(1), state.remainingDelay(now.plusSeconds(29)))
        state.recordRateLimit(retryAfter = Duration.ofSeconds(60), now = now.plusSeconds(1))
        assertTrue(state.isActive(now.plusSeconds(60)))
        assertFalse(state.isActive(now.plusSeconds(61)))
    }

    @Test
    fun `rate limit suppresses warmups without blocking excess idle shrink`() {
        val stateStore = InMemoryPoolStateStore()
        val poolName = "rate-limited-shrink"
        stateStore.putIdle(poolName, "idle-1")
        stateStore.putIdle(poolName, "idle-2")
        val config =
            PoolConfig.builder()
                .poolName(poolName)
                .ownerId("owner-1")
                .maxIdle(1)
                .warmupConcurrency(1)
                .stateStore(stateStore)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .build()
        val rateLimitState = PoolRateLimitState()
        rateLimitState.recordRateLimit(Duration.ofSeconds(30))
        val discarded = mutableListOf<String>()
        val submitted = AtomicInteger(0)

        PoolReconciler.runReconcileTick(
            config = config,
            stateStore = stateStore,
            onDiscardSandbox = { discarded += it },
            reconcileState = ReconcileState(degradedThreshold = 3),
            warmingCount = 0,
            rateLimitState = rateLimitState,
            submitWarmups = { submitted.addAndGet(it) },
        )

        assertEquals(1, discarded.size)
        assertEquals(0, submitted.get())
        assertEquals(1, stateStore.snapshotCounters(poolName).idleCount)
    }
}
