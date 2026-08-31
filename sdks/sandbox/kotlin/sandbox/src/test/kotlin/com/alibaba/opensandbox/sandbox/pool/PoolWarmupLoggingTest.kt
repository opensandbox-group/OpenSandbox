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

import ch.qos.logback.classic.Level
import ch.qos.logback.classic.Logger
import ch.qos.logback.classic.spi.ILoggingEvent
import ch.qos.logback.core.read.ListAppender
import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec
import com.alibaba.opensandbox.sandbox.domain.pool.PooledSandboxCreator
import com.alibaba.opensandbox.sandbox.infrastructure.pool.InMemoryPoolStateStore
import com.alibaba.opensandbox.sandbox.transport.RetryPolicy
import io.mockk.every
import io.mockk.mockk
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.slf4j.LoggerFactory
import java.time.Duration
import java.util.concurrent.atomic.AtomicInteger

class PoolWarmupLoggingTest {
    @Test
    fun `terminal and forced periodic summary logs use stable structured fields`() {
        withPoolAppender { events ->
            val store = InMemoryPoolStateStore()
            val pool = successfulPool("log-success-pool", store)
            pool.start()
            try {
                awaitCondition {
                    store.snapshotCounters("log-success-pool").idleCount == 1 &&
                        pool.snapshot().inFlightOperations == 0
                }
                pool.logPoolSummaryForTests()

                val terminal =
                    events.single {
                        it.formattedMessage.startsWith("Pool warmup terminal:") &&
                            it.formattedMessage.contains("pool_name=log-success-pool")
                    }
                assertEquals(Level.DEBUG, terminal.level)
                assertTrue(terminal.formattedMessage.contains("stage=commit result=success reason=none"))

                val summary =
                    events.single {
                        it.formattedMessage.startsWith("Pool warmup summary:") &&
                            it.formattedMessage.contains("pool_name=log-success-pool")
                    }
                assertEquals(Level.INFO, summary.level)
                assertTrue(
                    summary.formattedMessage.contains(
                        "snapshot_consistency=eventual idle_snapshot=1 inflight_current=0 delay_queue_size=0",
                    ),
                )
                assertTrue(summary.formattedMessage.contains("admission_attempts_delta=1 success_delta=1"))

                pool.logPoolSummaryForTests()
                assertEquals(
                    1,
                    events.count {
                        it.formattedMessage.startsWith("Pool warmup summary:") &&
                            it.formattedMessage.contains("pool_name=log-success-pool")
                    },
                    "an idle but inactive pool must not emit a periodic heartbeat",
                )
            } finally {
                pool.shutdown(graceful = false)
            }
        }
    }

    @Test
    fun `periodic summary reports delayed readiness without scanning tasks`() {
        withPoolAppender { events ->
            val created = AtomicInteger(0)
            val store = InMemoryPoolStateStore()
            val pool =
                SandboxPool.builder()
                    .poolName("log-delayed-pool")
                    .ownerId("log-owner")
                    .maxIdle(1)
                    .warmupConcurrency(1)
                    .stateStore(store)
                    .connectionConfig(
                        ConnectionConfig.builder()
                            .retryPolicy(RetryPolicy.disabled())
                            .build(),
                    )
                    .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                    .sandboxCreator(
                        PooledSandboxCreator {
                            created.incrementAndGet()
                            mockSandbox("log-delayed-1")
                        },
                    ).warmupHealthCheck { true }
                    .warmupHealthCheckInitialDelay(Duration.ofSeconds(30))
                    .drainTimeout(Duration.ofMillis(200))
                    .build()

            pool.start()
            try {
                awaitCondition { created.get() == 1 }
                pool.logPoolSummaryForTests()
                val summary =
                    events.single {
                        it.formattedMessage.startsWith("Pool warmup summary:") &&
                            it.formattedMessage.contains("pool_name=log-delayed-pool")
                    }
                assertTrue(summary.formattedMessage.contains("inflight_current=1 delay_queue_size=1"))
                assertTrue(summary.formattedMessage.contains("stage_readiness_approx=1"))
            } finally {
                pool.shutdown(graceful = false)
            }
        }
    }

    @Test
    fun `repeated failures are rate limited while summary retains exact deltas`() {
        withPoolAppender { events ->
            val pool =
                SandboxPool.builder()
                    .poolName("log-rate-limit-pool")
                    .ownerId("log-owner")
                    .maxIdle(1)
                    .warmupConcurrency(1)
                    .stateStore(InMemoryPoolStateStore())
                    .connectionConfig(ConnectionConfig.builder().build())
                    .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                    .sandboxCreator(PooledSandboxCreator { throw IllegalStateException("create boom") })
                    .warmupSkipHealthCheck()
                    .drainTimeout(Duration.ofSeconds(1))
                    .build()

            pool.start()
            try {
                awaitCondition { pool.snapshot().failureCount >= 2 }
                pool.logPoolSummaryForTests()
                val warnings =
                    events.filter {
                        it.level == Level.WARN &&
                            it.formattedMessage.startsWith("Pool warmup terminal:") &&
                            it.formattedMessage.contains("pool_name=log-rate-limit-pool")
                    }
                assertEquals(1, warnings.size)
                val warning = warnings.single()
                assertTrue(warning.formattedMessage.contains("error=create boom"))
                assertEquals(IllegalStateException::class.java.name, warning.throwableProxy.className)
                assertEquals("create boom", warning.throwableProxy.message)
                val summary =
                    events.single {
                        it.formattedMessage.startsWith("Pool warmup summary:") &&
                            it.formattedMessage.contains("pool_name=log-rate-limit-pool")
                    }
                assertTrue(summary.formattedMessage.contains("failure_delta=2"))
                assertTrue(summary.formattedMessage.contains("create_failed_delta=2"))
            } finally {
                pool.shutdown(graceful = false)
            }
        }
    }

    private fun successfulPool(
        poolName: String,
        store: InMemoryPoolStateStore,
    ): SandboxPool =
        SandboxPool.builder()
            .poolName(poolName)
            .ownerId("log-owner")
            .maxIdle(1)
            .warmupConcurrency(1)
            .stateStore(store)
            .connectionConfig(ConnectionConfig.builder().build())
            .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
            .sandboxCreator(PooledSandboxCreator { mockSandbox("log-success-1") })
            .warmupSkipHealthCheck()
            .drainTimeout(Duration.ofSeconds(1))
            .build()

    private fun mockSandbox(id: String): Sandbox =
        mockk<Sandbox>(relaxed = true).also { sandbox ->
            every { sandbox.id } returns id
        }

    private fun withPoolAppender(block: (List<ILoggingEvent>) -> Unit) {
        val logger = LoggerFactory.getLogger(SandboxPool::class.java) as Logger
        val previousLevel = logger.level
        val appender = ListAppender<ILoggingEvent>().also { it.start() }
        logger.level = Level.DEBUG
        logger.addAppender(appender)
        try {
            block(appender.list)
        } finally {
            logger.detachAppender(appender)
            appender.stop()
            logger.level = previousLevel
        }
    }

    private fun awaitCondition(
        timeout: Duration = Duration.ofSeconds(5),
        condition: () -> Boolean,
    ) {
        val deadline = System.nanoTime() + timeout.toNanos()
        while (System.nanoTime() < deadline) {
            if (condition()) return
            Thread.sleep(20)
        }
        throw AssertionError("condition not met within $timeout")
    }
}
