/*
 * Copyright 2025 Alibaba Group Holding Ltd.
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

package com.alibaba.opensandbox.sandbox.pool

import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxRateLimitException
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec
import com.alibaba.opensandbox.sandbox.domain.pool.PoolState
import com.alibaba.opensandbox.sandbox.domain.pool.PooledSandboxCreator
import com.alibaba.opensandbox.sandbox.infrastructure.pool.InMemoryPoolStateStore
import io.mockk.every
import io.mockk.mockk
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.time.Duration
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicLong

class SandboxPoolRateLimitTest {
    @Test
    fun `rate limited warmup honors retry after without degrading pool`() {
        val attempts = AtomicInteger(0)
        val firstAttemptAt = AtomicLong(0)
        val secondAttemptAt = AtomicLong(0)
        val sandbox = mockk<Sandbox>(relaxed = true)
        every { sandbox.id } returns "rate-limit-recovery"

        val pool =
            SandboxPool.builder()
                .poolName("rate-limited-pool")
                .ownerId("rate-limited-owner")
                .maxIdle(1)
                .warmupConcurrency(1)
                .degradedThreshold(1)
                .stateStore(InMemoryPoolStateStore())
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(
                    PooledSandboxCreator {
                        when (attempts.incrementAndGet()) {
                            1 -> {
                                firstAttemptAt.set(System.nanoTime())
                                throw SandboxRateLimitException(
                                    message = "rate limited",
                                    retryAfter = Duration.ofSeconds(1),
                                )
                            }
                            else -> {
                                secondAttemptAt.compareAndSet(0, System.nanoTime())
                                sandbox
                            }
                        }
                    },
                ).warmupSkipHealthCheck()
                .reconcileInterval(Duration.ofSeconds(30))
                .build()

        pool.start()
        try {
            awaitCondition { pool.snapshot().backoffActive }

            val throttled = pool.snapshot()
            assertEquals(PoolState.HEALTHY, throttled.state)
            assertEquals(0, throttled.failureCount)
            assertFalse(
                awaitCondition(timeout = Duration.ofMillis(250)) { attempts.get() > 1 },
                "warmup must not retry before Retry-After expires",
            )

            assertTrue(awaitCondition { pool.snapshot().idleCount == 1 })
            val retryDelay = Duration.ofNanos(secondAttemptAt.get() - firstAttemptAt.get())
            assertTrue(retryDelay >= Duration.ofMillis(800), "warmup retried too early: $retryDelay")
            assertEquals(2, attempts.get())
            assertFalse(pool.snapshot().backoffActive)
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `pool restart clears rate limit throttle from previous run`() {
        val attempts = AtomicInteger(0)
        val sandbox = mockk<Sandbox>(relaxed = true)
        every { sandbox.id } returns "restart-rate-limit-recovery"
        val pool =
            SandboxPool.builder()
                .poolName("restart-rate-limited-pool")
                .ownerId("restart-rate-limited-owner")
                .maxIdle(1)
                .warmupConcurrency(1)
                .stateStore(InMemoryPoolStateStore())
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(
                    PooledSandboxCreator {
                        if (attempts.incrementAndGet() == 1) {
                            throw SandboxRateLimitException(
                                message = "rate limited",
                                retryAfter = Duration.ofSeconds(30),
                            )
                        }
                        sandbox
                    },
                ).warmupSkipHealthCheck()
                .reconcileInterval(Duration.ofSeconds(30))
                .build()

        pool.start()
        assertTrue(awaitCondition { pool.snapshot().backoffActive })
        pool.shutdown(graceful = false)

        pool.start()
        try {
            assertFalse(pool.snapshot().backoffActive)
            assertTrue(awaitCondition { pool.snapshot().idleCount == 1 })
            assertEquals(2, attempts.get())
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    private fun awaitCondition(
        timeout: Duration = Duration.ofSeconds(5),
        condition: () -> Boolean,
    ): Boolean {
        val deadline = System.nanoTime() + timeout.toNanos()
        while (System.nanoTime() < deadline) {
            if (condition()) return true
            TimeUnit.MILLISECONDS.sleep(10)
        }
        return condition()
    }
}
