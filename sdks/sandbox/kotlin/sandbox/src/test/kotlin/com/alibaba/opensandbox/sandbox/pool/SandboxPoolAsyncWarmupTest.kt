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

package com.alibaba.opensandbox.sandbox.pool

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.pool.AcquirePolicy
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec
import com.alibaba.opensandbox.sandbox.domain.pool.PooledSandboxCreateContext
import com.alibaba.opensandbox.sandbox.domain.pool.PooledSandboxCreator
import com.alibaba.opensandbox.sandbox.domain.pool.SandboxPreparer
import com.alibaba.opensandbox.sandbox.infrastructure.pool.InMemoryPoolStateStore
import com.alibaba.opensandbox.sandbox.transport.RetryPolicy
import io.mockk.every
import io.mockk.mockk
import io.mockk.verify
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.time.Duration
import java.util.Collections
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicLong
import java.util.concurrent.atomic.AtomicReference

class SandboxPoolAsyncWarmupTest {
    @Test
    fun `readiness retries wait in delay queue before prepare`() {
        val sandbox = sandbox("delayed-readiness")
        val createdAt = AtomicLong(0)
        val healthAttempts = Collections.synchronizedList(mutableListOf<Long>())
        val prepareCalls = AtomicInteger(0)
        val pool =
            poolBuilder("delayed-readiness", sandbox)
                .warmupHealthCheckInitialDelay(Duration.ofMillis(150))
                .warmupHealthCheckPollingInterval(Duration.ofMillis(100))
                .warmupReadyTimeout(Duration.ofSeconds(2))
                .warmupHealthCheck {
                    healthAttempts += System.nanoTime()
                    healthAttempts.size >= 2
                }
                .warmupSandboxPreparer(SandboxPreparer { prepareCalls.incrementAndGet() })
                .sandboxCreator(
                    PooledSandboxCreator {
                        createdAt.set(System.nanoTime())
                        sandbox
                    },
                ).build()

        pool.start()
        try {
            awaitCondition { pool.snapshot().idleCount == 1 }

            assertEquals(2, healthAttempts.size)
            assertTrue(elapsed(createdAt.get(), healthAttempts[0]) >= Duration.ofMillis(100))
            assertTrue(elapsed(healthAttempts[0], healthAttempts[1]) >= Duration.ofMillis(70))
            assertEquals(1, prepareCalls.get())
            verify(exactly = 1) { sandbox.renew(Duration.ofHours(24)) }
            verify(exactly = 1) { sandbox.close() }
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `soft readiness deadline still executes one successful final check`() {
        val sandbox = sandbox("soft-deadline")
        val healthAttempts = AtomicInteger(0)
        val pool =
            poolBuilder("soft-deadline", sandbox)
                .warmupReadyTimeout(Duration.ofMillis(100))
                .warmupHealthCheckInitialDelay(Duration.ofSeconds(1))
                .warmupHealthCheck { healthAttempts.incrementAndGet() == 1 }
                .build()

        pool.start()
        try {
            awaitCondition { pool.snapshot().idleCount == 1 }
            assertEquals(1, healthAttempts.get())
            verify(exactly = 0) { sandbox.kill() }
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `post prepare retries do not rerun readiness or preparer`() {
        val sandbox = sandbox("post-prepare")
        val readinessCalls = AtomicInteger(0)
        val prepareCalls = AtomicInteger(0)
        val postPrepareCalls = AtomicInteger(0)
        val pool =
            poolBuilder("post-prepare", sandbox)
                .warmupHealthCheck { readinessCalls.incrementAndGet() == 1 }
                .warmupSandboxPreparer(SandboxPreparer { prepareCalls.incrementAndGet() })
                .warmupPostPrepareHealthCheck { postPrepareCalls.incrementAndGet() >= 2 }
                .warmupHealthCheckPollingInterval(Duration.ofMillis(50))
                .warmupPostPrepareHealthCheckTimeout(Duration.ofSeconds(1))
                .build()

        pool.start()
        try {
            awaitCondition { pool.snapshot().idleCount == 1 }
            assertEquals(1, readinessCalls.get())
            assertEquals(1, prepareCalls.get())
            assertEquals(2, postPrepareCalls.get())
            verify(exactly = 1) { sandbox.renew(Duration.ofHours(24)) }
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `warmup creator receives skip health check and a create-only single-attempt config`() {
        val sandbox = sandbox("creator-context")
        val contextRef = AtomicReference<PooledSandboxCreateContext>()
        val connectionConfig =
            ConnectionConfig.builder()
                .retryPolicy(RetryPolicy.disabled())
                .build()
        val pool =
            SandboxPool.builder()
                .poolName("creator-context")
                .ownerId("owner")
                .maxIdle(1)
                .warmupCreateQps(1)
                .warmupConcurrency(1)
                .stateStore(InMemoryPoolStateStore())
                .connectionConfig(connectionConfig)
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(
                    PooledSandboxCreator { context ->
                        contextRef.set(context)
                        sandbox
                    },
                ).warmupSkipHealthCheck()
                .build()

        pool.start()
        try {
            awaitCondition { pool.snapshot().idleCount == 1 }
            val context = contextRef.get()
            assertEquals(PooledSandboxCreateContext.Reason.WARMUP, context.reason)
            assertTrue(context.skipHealthCheck)
            assertTrue(context.connectionConfig.singleAttemptHealthChecks)
            HttpClientProvider(context.connectionConfig).use { provider ->
                assertTrue(provider.authenticatedClient.retryOnConnectionFailure)
            }
            HttpClientProvider(context.createConnectionConfig).use { provider ->
                assertFalse(provider.authenticatedClient.retryOnConnectionFailure)
            }
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `direct create keeps configured health and retry behavior`() {
        val direct = sandbox("direct-create")
        val contextRef = AtomicReference<PooledSandboxCreateContext>()
        val pool =
            SandboxPool.builder()
                .poolName("direct-create")
                .ownerId("owner")
                .maxIdle(0)
                .stateStore(InMemoryPoolStateStore())
                .connectionConfig(ConnectionConfig.builder().retryPolicy(RetryPolicy.disabled()).build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(
                    PooledSandboxCreator { context ->
                        contextRef.set(context)
                        direct
                    },
                ).build()

        pool.start()
        try {
            assertEquals(direct, pool.acquire(policy = AcquirePolicy.DIRECT_CREATE))
            val context = contextRef.get()
            assertEquals(PooledSandboxCreateContext.Reason.DIRECT_CREATE, context.reason)
            assertFalse(context.skipHealthCheck)
            assertFalse(context.connectionConfig.singleAttemptHealthChecks)
            HttpClientProvider(context.createConnectionConfig).use { provider ->
                assertTrue(provider.authenticatedClient.retryOnConnectionFailure)
            }
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    private fun poolBuilder(
        name: String,
        sandbox: Sandbox,
    ): SandboxPool.Builder =
        SandboxPool.builder()
            .poolName(name)
            .ownerId("owner")
            .maxIdle(1)
            .warmupCreateQps(1)
            .warmupConcurrency(1)
            .stateStore(InMemoryPoolStateStore())
            .connectionConfig(ConnectionConfig.builder().build())
            .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
            .sandboxCreator(PooledSandboxCreator { sandbox })

    private fun sandbox(id: String): Sandbox =
        mockk<Sandbox>(relaxed = true).also { sandbox ->
            every { sandbox.id } returns id
        }

    private fun elapsed(
        startNanos: Long,
        endNanos: Long,
    ): Duration = Duration.ofNanos(endNanos - startNanos)

    private fun awaitCondition(
        timeout: Duration = Duration.ofSeconds(5),
        condition: () -> Boolean,
    ) {
        val deadline = System.nanoTime() + timeout.toNanos()
        while (!condition()) {
            if (System.nanoTime() >= deadline) {
                throw AssertionError("Condition was not satisfied within $timeout")
            }
            TimeUnit.MILLISECONDS.sleep(10)
        }
    }
}
