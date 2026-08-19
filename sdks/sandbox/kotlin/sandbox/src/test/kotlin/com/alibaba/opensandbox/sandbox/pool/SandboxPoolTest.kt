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
import com.alibaba.opensandbox.sandbox.SandboxManager
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolAcquireFailedException
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolEmptyException
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolNotRunningException
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialProxyConfig
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Host
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PlatformSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Volume
import com.alibaba.opensandbox.sandbox.domain.pool.AcquirePolicy
import com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry
import com.alibaba.opensandbox.sandbox.domain.pool.PoolConfig
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec
import com.alibaba.opensandbox.sandbox.domain.pool.PoolDestroyState
import com.alibaba.opensandbox.sandbox.domain.pool.PoolLifecycleState
import com.alibaba.opensandbox.sandbox.domain.pool.PoolState
import com.alibaba.opensandbox.sandbox.domain.pool.PoolStateStore
import com.alibaba.opensandbox.sandbox.domain.pool.PooledSandboxCreator
import com.alibaba.opensandbox.sandbox.domain.pool.SandboxPreparer
import com.alibaba.opensandbox.sandbox.domain.pool.StoreCounters
import com.alibaba.opensandbox.sandbox.domain.pool.TakeIdleResult
import com.alibaba.opensandbox.sandbox.infrastructure.pool.InMemoryPoolStateStore
import io.mockk.every
import io.mockk.just
import io.mockk.mockk
import io.mockk.runs
import io.mockk.verify
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertSame
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.io.InterruptedIOException
import java.time.Duration
import java.time.Instant
import java.util.concurrent.CountDownLatch
import java.util.concurrent.ExecutionException
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit
import java.util.concurrent.TimeoutException
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicReference

class SandboxPoolTest {
    @Test
    fun `snapshot before start returns STOPPED and zero idle`() {
        val pool = buildPool()
        val snap = pool.snapshot()
        assertEquals(PoolState.STOPPED, snap.state)
        assertEquals(PoolLifecycleState.NOT_STARTED, snap.lifecycleState)
        assertEquals(0, snap.idleCount)
        assertEquals(2, snap.maxIdle)
        assertEquals(0, snap.failureCount)
        assertEquals(false, snap.backoffActive)
        assertEquals(0, snap.inFlightOperations)
    }

    @Test
    fun `start then snapshot returns RUNNING`() {
        val pool = buildPool()
        pool.start()
        try {
            val snap = pool.snapshot()
            assertEquals(PoolState.HEALTHY, snap.state)
            assertEquals(PoolLifecycleState.RUNNING, snap.lifecycleState)
            assertEquals(2, snap.maxIdle)
            assertTrue(snap.failureCount >= 0)
            assertTrue(snap.inFlightOperations >= 0)
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `snapshot reports in flight operations`() {
        val pool = buildPool()
        pool.start()
        val inFlight = currentRunInFlight(pool)
        inFlight.set(3)
        val snap =
            try {
                pool.snapshot()
            } finally {
                inFlight.set(0)
                pool.shutdown(graceful = false)
            }

        assertEquals(3, snap.inFlightOperations)
    }

    @Test
    fun `resize updates maxIdle`() {
        val pool = buildPool()
        pool.start()
        try {
            pool.resize(10)
            val snap = pool.snapshot()
            assertEquals(PoolState.HEALTHY, snap.state)
            assertEquals(10, snap.maxIdle)
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `shutdown graceful then snapshot returns STOPPED`() {
        val pool = buildPool()
        pool.start()
        pool.shutdown(graceful = true)
        val snap = pool.snapshot()
        assertEquals(PoolState.STOPPED, snap.state)
        assertEquals(PoolLifecycleState.STOPPED, snap.lifecycleState)
    }

    @Test
    fun `rolling warmup fills a released slot without waiting for slow tail`() {
        val store = InMemoryPoolStateStore()
        val created = AtomicInteger(0)
        val active = AtomicInteger(0)
        val maxActive = AtomicInteger(0)
        val slowWarmupStarted = CountDownLatch(1)
        val releaseSlowWarmup = CountDownLatch(1)
        val thirdWarmupStarted = CountDownLatch(1)

        val pool =
            SandboxPool.builder()
                .poolName("rolling-pool")
                .ownerId("rolling-owner")
                .maxIdle(4)
                .warmupConcurrency(2)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(
                    PooledSandboxCreator {
                        val index = created.incrementAndGet()
                        mockk<Sandbox>(relaxed = true).also { sandbox ->
                            every { sandbox.id } returns "rolling-$index"
                        }
                    },
                ).warmupSkipHealthCheck()
                .warmupSandboxPreparer(
                    SandboxPreparer { sandbox ->
                        val currentActive = active.incrementAndGet()
                        maxActive.updateAndGet { current -> maxOf(current, currentActive) }
                        try {
                            when (sandbox.id) {
                                "rolling-1" -> {
                                    slowWarmupStarted.countDown()
                                    releaseSlowWarmup.await()
                                }
                                "rolling-3" -> thirdWarmupStarted.countDown()
                            }
                        } finally {
                            active.decrementAndGet()
                        }
                    },
                ).reconcileInterval(Duration.ofSeconds(30))
                .drainTimeout(Duration.ofSeconds(2))
                .build()

        pool.start()
        try {
            assertTrue(slowWarmupStarted.await(5, TimeUnit.SECONDS))
            assertTrue(
                thirdWarmupStarted.await(5, TimeUnit.SECONDS),
                "a fast completion should refill its slot before the slow first warmup finishes",
            )
            assertEquals(1L, releaseSlowWarmup.count, "slow warmup must still be blocked")
            assertTrue(maxActive.get() <= 2, "rolling warmup must respect warmupConcurrency")

            releaseSlowWarmup.countDown()
            awaitCondition { store.snapshotCounters("rolling-pool").idleCount == 4 }
            assertEquals(4, created.get())
        } finally {
            releaseSlowWarmup.countDown()
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `failed warmup does not trigger an immediate completion-driven reconcile`() {
        val store = CountingPoolStateStore()
        val created = AtomicInteger(0)
        val pool =
            SandboxPool.builder()
                .poolName("failure-no-retrigger-pool")
                .ownerId("failure-no-retrigger-owner")
                .maxIdle(2)
                .warmupConcurrency(1)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(
                    PooledSandboxCreator {
                        created.incrementAndGet()
                        throw RuntimeException("fast create failure")
                    },
                ).warmupSkipHealthCheck()
                .reconcileInterval(Duration.ofSeconds(30))
                .drainTimeout(Duration.ofMillis(200))
                .build()

        pool.start()
        try {
            awaitCondition { pool.snapshot().failureCount >= 1 }
            // Fast-failing warmups previously queued a new reconcile tick on every failure,
            // causing unbounded create/retry churn within a single reconcileInterval.
            Thread.sleep(800)
            assertEquals(1, created.get(), "failed warmup must not be retried before the periodic tick")
            assertTrue(
                store.reconcileTicks.get() <= 2,
                "failed warmup must not drive extra reconcile ticks, got=${store.reconcileTicks.get()}",
            )
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `completion-driven reconcile ticks are rate limited during fast completions`() {
        val store = CountingPoolStateStore()
        val created = AtomicInteger(0)
        val pool =
            SandboxPool.builder()
                .poolName("tick-rate-limit-pool")
                .ownerId("tick-rate-limit-owner")
                .maxIdle(4)
                .warmupConcurrency(2)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(
                    PooledSandboxCreator {
                        if (created.getAndIncrement() % 2 == 0) {
                            throw RuntimeException("fast create failure")
                        }
                        mockk<Sandbox>(relaxed = true).also { sandbox ->
                            every { sandbox.id } returns "alternating-${created.get()}"
                        }
                    },
                ).warmupSkipHealthCheck()
                .reconcileInterval(Duration.ofSeconds(30))
                .drainTimeout(Duration.ofMillis(200))
                .build()

        pool.start()
        try {
            // A mixed burst of fast success/failure completions keeps the pool in deficit and
            // previously drove one reconcile tick per completion round (~tens per second),
            // amplified by several state-store round-trips per tick. The minimum-interval
            // coalescing window must cap the tick rate regardless of outcome mix.
            Thread.sleep(1100)
            assertTrue(
                store.reconcileTicks.get() <= 5,
                "completion-driven ticks must be rate limited, got=${store.reconcileTicks.get()}",
            )
            assertTrue(created.get() >= 3, "burst must still make progress, created=${created.get()}")
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `primary heartbeat continues while warmup is blocked`() {
        val store = HeartbeatRecordingStore()
        val sandbox = mockk<Sandbox>(relaxed = true)
        val warmupStarted = CountDownLatch(1)
        val releaseWarmup = CountDownLatch(1)
        every { sandbox.id } returns "heartbeat-warmup"

        val pool =
            SandboxPool.builder()
                .poolName("heartbeat-pool")
                .ownerId("heartbeat-owner")
                .maxIdle(1)
                .warmupConcurrency(1)
                .primaryLockTtl(Duration.ofMillis(300))
                .reconcileInterval(Duration.ofSeconds(30))
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(PooledSandboxCreator { sandbox })
                .warmupSkipHealthCheck()
                .warmupSandboxPreparer(
                    SandboxPreparer {
                        warmupStarted.countDown()
                        releaseWarmup.await()
                    },
                ).drainTimeout(Duration.ofSeconds(2))
                .build()

        pool.start()
        try {
            assertTrue(warmupStarted.await(5, TimeUnit.SECONDS))
            awaitCondition { store.renewCalls.get() >= 3 }
            assertEquals(1L, releaseWarmup.count, "warmup must remain blocked while heartbeat renews")
        } finally {
            releaseWarmup.countDown()
            pool.shutdown(graceful = true)
        }
    }

    @Test
    fun `warmup result is killed instead of entering idle after primary lock loss`() {
        val store = RenewFailsAfterAdmissionStore()
        val manager = mockk<SandboxManager>(relaxed = true)
        val sandbox = mockk<Sandbox>(relaxed = true)
        val killed = CountDownLatch(1)
        every { sandbox.id } returns "lost-lock-warmup"
        every { manager.killSandbox("lost-lock-warmup") } answers {
            killed.countDown()
        }
        val pool =
            SandboxPool(
                config =
                    PoolConfig.builder()
                        .poolName("lost-lock-pool")
                        .ownerId("lost-lock-owner")
                        .maxIdle(1)
                        .warmupConcurrency(1)
                        .stateStore(store)
                        .connectionConfig(ConnectionConfig.builder().build())
                        .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                        .sandboxCreator(PooledSandboxCreator { sandbox })
                        .warmupSkipHealthCheck()
                        .reconcileInterval(Duration.ofSeconds(30))
                        .build(),
                sandboxManagerFactory = { manager },
            )

        pool.start()
        try {
            assertTrue(killed.await(5, TimeUnit.SECONDS))
            assertEquals(0, store.snapshotCounters("lost-lock-pool").idleCount)
            verify(exactly = 1) { manager.killSandbox("lost-lock-warmup") }
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `warmup put failure records backoff and cleans remote sandbox`() {
        val store = PutIdleFailureStore()
        val manager = mockk<SandboxManager>(relaxed = true)
        val sandbox = mockk<Sandbox>(relaxed = true)
        val killed = CountDownLatch(1)
        every { sandbox.id } returns "put-failed-warmup"
        every { manager.killSandbox("put-failed-warmup") } answers {
            killed.countDown()
        }
        val pool =
            SandboxPool(
                config =
                    PoolConfig.builder()
                        .poolName("put-failed-pool")
                        .ownerId("put-failed-owner")
                        .maxIdle(1)
                        .warmupConcurrency(1)
                        .degradedThreshold(1)
                        .stateStore(store)
                        .connectionConfig(ConnectionConfig.builder().build())
                        .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                        .sandboxCreator(PooledSandboxCreator { sandbox })
                        .warmupSkipHealthCheck()
                        .reconcileInterval(Duration.ofSeconds(30))
                        .build(),
                sandboxManagerFactory = { manager },
            )

        pool.start()
        try {
            assertTrue(killed.await(5, TimeUnit.SECONDS))
            awaitCondition { pool.snapshot().backoffActive }
            assertEquals(PoolState.DEGRADED, pool.snapshot().state)
            assertEquals(0, store.snapshotCounters("put-failed-pool").idleCount)
            verify(exactly = 1) { manager.killSandbox("put-failed-warmup") }
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `shutdown graceful waits for in-flight warmup without interrupting it`() {
        val store = InMemoryPoolStateStore()
        val sandbox = mockk<Sandbox>(relaxed = true)
        val warmupStarted = CountDownLatch(1)
        val releaseWarmup = CountDownLatch(1)
        val warmupInterrupted = AtomicBoolean(false)
        every { sandbox.id } returns "warmup-id"

        val pool =
            SandboxPool.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(1)
                .warmupConcurrency(1)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(PooledSandboxCreator { sandbox })
                .warmupSkipHealthCheck()
                .warmupSandboxPreparer(
                    SandboxPreparer {
                        warmupStarted.countDown()
                        try {
                            releaseWarmup.await()
                        } catch (e: InterruptedException) {
                            warmupInterrupted.set(true)
                            throw e
                        }
                    },
                ).drainTimeout(Duration.ofSeconds(2))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()

        var releaser: Thread? = null
        pool.start()
        try {
            assertTrue(warmupStarted.await(5, TimeUnit.SECONDS))
            releaser =
                Thread {
                    Thread.sleep(250)
                    releaseWarmup.countDown()
                }.apply { start() }

            pool.shutdown(graceful = true)

            assertEquals(false, warmupInterrupted.get())
            assertEquals(1, store.snapshotCounters("test-pool").idleCount)
            verify(exactly = 0) { sandbox.kill() }
            verify(exactly = 1) { sandbox.close() }
        } finally {
            releaseWarmup.countDown()
            releaser?.join(5_000)
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `shutdown graceful force interrupts warmup after drain timeout`() {
        val store = InMemoryPoolStateStore()
        val sandbox = mockk<Sandbox>(relaxed = true)
        val warmupStarted = CountDownLatch(1)
        val blockWarmup = CountDownLatch(1)
        val warmupInterrupted = AtomicBoolean(false)
        val killSawInterrupt = AtomicBoolean(false)
        val closeSawInterrupt = AtomicBoolean(false)
        every { sandbox.id } returns "timed-out-warmup-id"
        every { sandbox.kill() } answers {
            killSawInterrupt.set(Thread.currentThread().isInterrupted)
            if (killSawInterrupt.get()) {
                throw InterruptedIOException("interrupted")
            }
        }
        every { sandbox.close() } answers {
            closeSawInterrupt.set(Thread.currentThread().isInterrupted)
        }

        val pool =
            SandboxPool.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(1)
                .warmupConcurrency(1)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(PooledSandboxCreator { sandbox })
                .warmupSkipHealthCheck()
                .warmupSandboxPreparer(
                    SandboxPreparer {
                        warmupStarted.countDown()
                        try {
                            blockWarmup.await()
                        } catch (e: InterruptedException) {
                            warmupInterrupted.set(true)
                            Thread.currentThread().interrupt()
                            throw e
                        }
                    },
                ).drainTimeout(Duration.ofMillis(200))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()

        pool.start()
        try {
            assertTrue(warmupStarted.await(5, TimeUnit.SECONDS))
            val shutdownStartedAt = System.nanoTime()

            pool.shutdown(graceful = true)

            val shutdownElapsedMs = Duration.ofNanos(System.nanoTime() - shutdownStartedAt).toMillis()
            assertTrue(shutdownElapsedMs >= 150, "graceful shutdown should wait for drain timeout before forcing stop")
            assertEquals(true, warmupInterrupted.get())
            assertEquals(false, killSawInterrupt.get(), "cleanup kill must not inherit the worker interrupt state")
            assertEquals(true, closeSawInterrupt.get(), "worker interrupt state must be restored after cleanup kill")
            assertEquals(0, store.snapshotCounters("test-pool").idleCount)
            verify(exactly = 1) { sandbox.kill() }
            verify(exactly = 1) { sandbox.close() }
        } finally {
            blockWarmup.countDown()
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `warmup success from retired run is killed and cannot enter restarted pool`() {
        val store = InMemoryPoolStateStore()
        val manager = mockk<SandboxManager>(relaxed = true)
        val oldSandbox = mockk<Sandbox>(relaxed = true)
        val newSandbox = mockk<Sandbox>(relaxed = true)
        val createCount = AtomicInteger(0)
        val oldWarmupStarted = CountDownLatch(1)
        val releaseOldWarmup = CountDownLatch(1)
        val oldSandboxKilled = CountDownLatch(1)
        every { oldSandbox.id } returns "old-run-sandbox"
        every { newSandbox.id } returns "new-run-sandbox"
        every { manager.killSandbox("old-run-sandbox") } answers {
            oldSandboxKilled.countDown()
        }

        val pool =
            SandboxPool(
                config =
                    PoolConfig.builder()
                        .poolName("restart-pool")
                        .ownerId("test-owner")
                        .maxIdle(1)
                        .warmupConcurrency(1)
                        .stateStore(store)
                        .connectionConfig(ConnectionConfig.builder().build())
                        .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                        .sandboxCreator(
                            PooledSandboxCreator {
                                if (createCount.getAndIncrement() == 0) oldSandbox else newSandbox
                            },
                        ).warmupSkipHealthCheck()
                        .warmupSandboxPreparer(
                            SandboxPreparer { sandbox ->
                                if (sandbox === oldSandbox) {
                                    oldWarmupStarted.countDown()
                                    awaitIgnoringInterrupt(releaseOldWarmup)
                                }
                            },
                        ).drainTimeout(Duration.ofMillis(50))
                        .reconcileInterval(Duration.ofSeconds(30))
                        .build(),
                sandboxManagerFactory = { manager },
            )

        pool.start()
        try {
            assertTrue(oldWarmupStarted.await(5, TimeUnit.SECONDS))
            val retiredRunInFlight = currentRunInFlight(pool)
            shutdownWithoutWaitingForUncooperativeWorker(pool)

            pool.start()
            awaitCondition {
                pool.snapshotIdleEntries().map { it.sandboxId } == listOf("new-run-sandbox")
            }
            awaitCondition { pool.snapshot().inFlightOperations == 0 }

            releaseOldWarmup.countDown()
            assertTrue(oldSandboxKilled.await(5, TimeUnit.SECONDS))
            awaitCondition { retiredRunInFlight.get() == 0 }

            assertEquals(listOf("new-run-sandbox"), pool.snapshotIdleEntries().map { it.sandboxId })
            verify(exactly = 1) { manager.killSandbox("old-run-sandbox") }
        } finally {
            releaseOldWarmup.countDown()
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `warmup success after shutdown uses temporary manager for cleanup`() {
        val store = InMemoryPoolStateStore()
        val runningManager = mockk<SandboxManager>(relaxed = true)
        val cleanupManager = mockk<SandboxManager>(relaxed = true)
        val managerCount = AtomicInteger(0)
        val sandbox = mockk<Sandbox>(relaxed = true)
        val warmupStarted = CountDownLatch(1)
        val releaseWarmup = CountDownLatch(1)
        val sandboxKilled = CountDownLatch(1)
        every { sandbox.id } returns "late-warmup-sandbox"
        every { cleanupManager.killSandbox("late-warmup-sandbox") } answers {
            sandboxKilled.countDown()
        }

        val pool =
            SandboxPool(
                config =
                    PoolConfig.builder()
                        .poolName("shutdown-cleanup-pool")
                        .ownerId("test-owner")
                        .maxIdle(1)
                        .warmupConcurrency(1)
                        .stateStore(store)
                        .connectionConfig(ConnectionConfig.builder().build())
                        .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                        .sandboxCreator(PooledSandboxCreator { sandbox })
                        .warmupSkipHealthCheck()
                        .warmupSandboxPreparer(
                            SandboxPreparer {
                                warmupStarted.countDown()
                                awaitIgnoringInterrupt(releaseWarmup)
                            },
                        ).drainTimeout(Duration.ofMillis(50))
                        .reconcileInterval(Duration.ofSeconds(30))
                        .build(),
                sandboxManagerFactory = {
                    if (managerCount.getAndIncrement() == 0) runningManager else cleanupManager
                },
            )

        pool.start()
        try {
            assertTrue(warmupStarted.await(5, TimeUnit.SECONDS))
            val retiredRunInFlight = currentRunInFlight(pool)
            shutdownWithoutWaitingForUncooperativeWorker(pool)
            awaitCondition { pool.snapshot().inFlightOperations == 0 }

            releaseWarmup.countDown()
            assertTrue(sandboxKilled.await(5, TimeUnit.SECONDS))
            awaitCondition { retiredRunInFlight.get() == 0 }

            assertEquals(emptyList<String>(), pool.snapshotIdleEntries().map { it.sandboxId })
            verify(exactly = 1) { cleanupManager.killSandbox("late-warmup-sandbox") }
            verify(exactly = 1) { cleanupManager.close() }
        } finally {
            releaseWarmup.countDown()
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `warmup failure from retired run cannot degrade restarted pool`() {
        val store = InMemoryPoolStateStore()
        val oldSandbox = mockk<Sandbox>(relaxed = true)
        val newSandbox = mockk<Sandbox>(relaxed = true)
        val createCount = AtomicInteger(0)
        val oldWarmupStarted = CountDownLatch(1)
        val releaseOldWarmup = CountDownLatch(1)
        every { oldSandbox.id } returns "old-failed-sandbox"
        every { newSandbox.id } returns "new-run-sandbox"

        val pool =
            SandboxPool.builder()
                .poolName("restart-failure-pool")
                .ownerId("test-owner")
                .maxIdle(1)
                .warmupConcurrency(1)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(
                    PooledSandboxCreator {
                        if (createCount.getAndIncrement() == 0) oldSandbox else newSandbox
                    },
                ).warmupSkipHealthCheck()
                .warmupSandboxPreparer(
                    SandboxPreparer { sandbox ->
                        if (sandbox === oldSandbox) {
                            oldWarmupStarted.countDown()
                            awaitIgnoringInterrupt(releaseOldWarmup)
                            throw RuntimeException("retired run warmup failed")
                        }
                    },
                ).degradedThreshold(1)
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()

        pool.start()
        try {
            assertTrue(oldWarmupStarted.await(5, TimeUnit.SECONDS))
            val retiredRunInFlight = currentRunInFlight(pool)
            shutdownWithoutWaitingForUncooperativeWorker(pool)

            pool.start()
            awaitCondition {
                pool.snapshotIdleEntries().map { it.sandboxId } == listOf("new-run-sandbox")
            }
            awaitCondition { pool.snapshot().inFlightOperations == 0 }

            releaseOldWarmup.countDown()
            awaitCondition { retiredRunInFlight.get() == 0 }

            assertEquals(0, pool.snapshot().failureCount)
            assertEquals(false, pool.snapshot().backoffActive)
            assertEquals(PoolState.HEALTHY, pool.snapshot().state)
            verify(exactly = 1) { oldSandbox.kill() }
        } finally {
            releaseOldWarmup.countDown()
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `forced scheduler shutdown completes queued warmup outcome and cleans sandbox`() {
        val store = InMemoryPoolStateStore()
        val manager = mockk<SandboxManager>(relaxed = true)
        val sandbox = mockk<Sandbox>(relaxed = true)
        val warmupStarted = CountDownLatch(1)
        val releaseWarmup = CountDownLatch(1)
        val warmupClosed = CountDownLatch(1)
        val controllerBlocked = CountDownLatch(1)
        val releaseController = CountDownLatch(1)
        val sandboxKilled = CountDownLatch(1)
        every { sandbox.id } returns "queued-completion-sandbox"
        every { sandbox.close() } answers {
            warmupClosed.countDown()
        }
        every { manager.killSandbox("queued-completion-sandbox") } answers {
            sandboxKilled.countDown()
        }

        val pool =
            SandboxPool(
                config =
                    PoolConfig.builder()
                        .poolName("queued-completion-pool")
                        .ownerId("test-owner")
                        .maxIdle(1)
                        .warmupConcurrency(1)
                        .stateStore(store)
                        .connectionConfig(ConnectionConfig.builder().build())
                        .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                        .sandboxCreator(PooledSandboxCreator { sandbox })
                        .warmupSkipHealthCheck()
                        .warmupSandboxPreparer(
                            SandboxPreparer {
                                warmupStarted.countDown()
                                releaseWarmup.await()
                            },
                        ).drainTimeout(Duration.ofMillis(50))
                        .reconcileInterval(Duration.ofSeconds(30))
                        .build(),
                sandboxManagerFactory = { manager },
            )

        pool.start()
        try {
            assertTrue(warmupStarted.await(5, TimeUnit.SECONDS))
            val retiredRunInFlight = currentRunInFlight(pool)
            val pendingCompletions = currentRunPendingWarmupCompletions(pool)
            val controller = getPrivateField<ScheduledExecutorService>(pool, "scheduler")
            controller.execute {
                controllerBlocked.countDown()
                awaitIgnoringInterrupt(releaseController)
            }
            assertTrue(controllerBlocked.await(5, TimeUnit.SECONDS))

            releaseWarmup.countDown()
            assertTrue(warmupClosed.await(5, TimeUnit.SECONDS))
            awaitCondition { pendingCompletions.size == 1 }
            assertEquals(1, retiredRunInFlight.get())
            assertEquals(0, store.snapshotCounters("queued-completion-pool").idleCount)

            shutdownWithoutWaitingForUncooperativeWorker(pool)

            assertTrue(sandboxKilled.await(5, TimeUnit.SECONDS))
            awaitCondition { retiredRunInFlight.get() == 0 }
            assertEquals(0, store.snapshotCounters("queued-completion-pool").idleCount)
            verify(exactly = 1) { manager.killSandbox("queued-completion-sandbox") }
        } finally {
            releaseWarmup.countDown()
            releaseController.countDown()
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `shutdown graceful waits for asynchronous discarded sandbox cleanup`() {
        val store = DiscardingPoolStateStore()
        val manager = mockk<SandboxManager>(relaxed = true)
        val directSandbox = mockk<Sandbox>(relaxed = true)
        val cleanupStarted = CountDownLatch(1)
        val releaseCleanup = CountDownLatch(1)
        val cleanupInterrupted = AtomicBoolean(false)
        val shutdownCompleted = CountDownLatch(1)
        val shutdownFailure = AtomicReference<Throwable?>()
        every { manager.killSandbox("near-expiry-id") } answers {
            cleanupStarted.countDown()
            try {
                releaseCleanup.await()
            } catch (e: InterruptedException) {
                cleanupInterrupted.set(true)
                throw e
            }
        }
        val pool =
            buildDiscardedCleanupPool(
                store = store,
                manager = manager,
                directSandbox = directSandbox,
                drainTimeout = Duration.ofSeconds(2),
            )

        var shutdownThread: Thread? = null
        pool.start()
        try {
            assertSame(directSandbox, pool.acquire(policy = AcquirePolicy.DIRECT_CREATE))
            assertTrue(cleanupStarted.await(5, TimeUnit.SECONDS))
            assertEquals(1, pool.snapshot().inFlightOperations)

            shutdownThread =
                Thread {
                    try {
                        pool.shutdown(graceful = true)
                    } catch (t: Throwable) {
                        shutdownFailure.set(t)
                    } finally {
                        shutdownCompleted.countDown()
                    }
                }.apply { start() }

            assertEquals(
                false,
                shutdownCompleted.await(200, TimeUnit.MILLISECONDS),
                "graceful shutdown must keep waiting while asynchronous cleanup is running",
            )
            releaseCleanup.countDown()
            assertTrue(shutdownCompleted.await(5, TimeUnit.SECONDS))

            assertEquals(null, shutdownFailure.get())
            assertEquals(false, cleanupInterrupted.get())
            assertEquals(0, pool.snapshot().inFlightOperations)
            assertEquals(PoolLifecycleState.STOPPED, pool.snapshot().lifecycleState)
            verify(exactly = 1) { manager.killSandbox("near-expiry-id") }
        } finally {
            releaseCleanup.countDown()
            shutdownThread?.join(5_000)
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `shutdown timeout completes drain count for queued discarded sandbox cleanup`() {
        val store = DiscardingPoolStateStore()
        val manager = mockk<SandboxManager>(relaxed = true)
        val directSandbox = mockk<Sandbox>(relaxed = true)
        val blockerStarted = CountDownLatch(1)
        val releaseBlocker = CountDownLatch(1)
        val blockerInterrupted = AtomicBoolean(false)
        val pool =
            buildDiscardedCleanupPool(
                store = store,
                manager = manager,
                directSandbox = directSandbox,
                drainTimeout = Duration.ofMillis(200),
            )

        pool.start()
        try {
            val warmupExecutor = getPrivateField<ExecutorService>(pool, "warmupExecutor")
            warmupExecutor.execute {
                blockerStarted.countDown()
                try {
                    releaseBlocker.await()
                } catch (_: InterruptedException) {
                    blockerInterrupted.set(true)
                }
            }
            assertTrue(blockerStarted.await(5, TimeUnit.SECONDS))
            assertSame(directSandbox, pool.acquire(policy = AcquirePolicy.DIRECT_CREATE))
            assertEquals(1, pool.snapshot().inFlightOperations)
            val shutdownStartedAt = System.nanoTime()

            pool.shutdown(graceful = true)

            val shutdownElapsedMs = Duration.ofNanos(System.nanoTime() - shutdownStartedAt).toMillis()
            assertTrue(shutdownElapsedMs >= 150, "shutdown should wait for drain timeout before dropping queued cleanup")
            assertEquals(true, blockerInterrupted.get())
            assertEquals(0, pool.snapshot().inFlightOperations)
            verify(exactly = 0) { manager.killSandbox("near-expiry-id") }
        } finally {
            releaseBlocker.countDown()
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `rejected discarded sandbox cleanup runs inline without leaking drain count`() {
        val store = DiscardingPoolStateStore()
        val manager = mockk<SandboxManager>(relaxed = true)
        val directSandbox = mockk<Sandbox>(relaxed = true)
        every { manager.killSandbox("near-expiry-id") } throws RuntimeException("kill failed")
        val pool =
            buildDiscardedCleanupPool(
                store = store,
                manager = manager,
                directSandbox = directSandbox,
                drainTimeout = Duration.ofSeconds(2),
            )

        pool.start()
        try {
            getPrivateField<ExecutorService>(pool, "warmupExecutor").shutdownNow()

            assertSame(directSandbox, pool.acquire(policy = AcquirePolicy.DIRECT_CREATE))

            assertEquals(0, pool.snapshot().inFlightOperations)
            verify(exactly = 1) { manager.killSandbox("near-expiry-id") }
        } finally {
            pool.shutdown(graceful = true)
        }
    }

    @Test
    fun `shutdown non-graceful then snapshot returns STOPPED`() {
        val pool = buildPool()
        pool.start()
        pool.shutdown(graceful = false)
        val snap = pool.snapshot()
        assertEquals(PoolState.STOPPED, snap.state)
        assertEquals(PoolLifecycleState.STOPPED, snap.lifecycleState)
    }

    @Test
    fun `shutdown graceful releases primary lock best effort`() {
        val store = RecordingPoolStateStore()
        val pool = buildPool(store = store, maxIdle = 0)

        pool.start()
        pool.shutdown(graceful = true)

        assertEquals(listOf("test-pool" to "test-owner"), store.releasedLocks)
    }

    @Test
    fun `shutdown non-graceful releases primary lock best effort`() {
        val store = RecordingPoolStateStore()
        val pool = buildPool(store = store, maxIdle = 0)

        pool.start()
        pool.shutdown(graceful = false)

        assertEquals(listOf("test-pool" to "test-owner"), store.releasedLocks)
    }

    @Test
    fun `shutdown completes when primary lock release fails`() {
        val store = RecordingPoolStateStore(releaseFails = true)
        val pool = buildPool(store = store, maxIdle = 0)

        pool.start()
        pool.shutdown(graceful = true)

        val snap = pool.snapshot()
        assertEquals(PoolLifecycleState.STOPPED, snap.lifecycleState)
        assertEquals(listOf("test-pool" to "test-owner"), store.releasedLocks)
    }

    @Test
    fun `acquire with FAIL_FAST and empty idle throws PoolEmptyException`() {
        val pool = buildPool()
        pool.start()
        try {
            assertThrows(PoolEmptyException::class.java) {
                pool.acquire(policy = AcquirePolicy.FAIL_FAST)
            }
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `acquire with FAIL_FAST and stale idle throws PoolAcquireFailedException`() {
        val store = InMemoryPoolStateStore()
        val pool =
            SandboxPool.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(2)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()
        store.putIdle("test-pool", "non-existent-id")

        pool.start()
        try {
            assertThrows(PoolAcquireFailedException::class.java) {
                pool.acquire(policy = AcquirePolicy.FAIL_FAST)
            }
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `stale acquire cleanup triggers replenish before periodic reconcile`() {
        val store = CountingPoolStateStore()
        val manager = mockk<SandboxManager>(relaxed = true)
        val created = AtomicInteger(0)
        val killed = CountDownLatch(1)
        every { manager.killSandbox("warmup-1") } answers { killed.countDown() }

        val config =
            PoolConfig.builder()
                .poolName("cleanup-reconcile-pool")
                .ownerId("cleanup-reconcile-owner")
                .maxIdle(1)
                .warmupConcurrency(1)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(
                    PooledSandboxCreator {
                        val index = created.incrementAndGet()
                        mockk<Sandbox>(relaxed = true).also { sandbox ->
                            every { sandbox.id } returns "warmup-$index"
                        }
                    },
                ).warmupSkipHealthCheck()
                .reconcileInterval(Duration.ofSeconds(30))
                .drainTimeout(Duration.ofSeconds(2))
                .build()
        val pool =
            SandboxPool(
                config = config,
                sandboxManagerFactory = { manager },
                idleSandboxConnector = { throw RuntimeException("stale sandbox") },
            )

        pool.start()
        try {
            awaitCondition {
                store.snapshotCounters("cleanup-reconcile-pool").idleCount == 1 &&
                    store.reconcileTicks.get() >= 2
            }

            assertThrows(PoolAcquireFailedException::class.java) {
                pool.acquire(policy = AcquirePolicy.FAIL_FAST)
            }

            assertTrue(killed.await(5, TimeUnit.SECONDS))
            awaitCondition {
                created.get() == 2 && store.snapshotCounters("cleanup-reconcile-pool").idleCount == 1
            }
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `acquire with RETRY_NEXT_IDLE and empty idle throws PoolEmptyException`() {
        val pool = buildPool()
        pool.start()
        try {
            val ex =
                assertThrows(PoolEmptyException::class.java) {
                    pool.acquire(policy = AcquirePolicy.RETRY_NEXT_IDLE)
                }
            assertTrue(ex.message?.contains("RETRY_NEXT_IDLE") == true)
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `acquire with RETRY_NEXT_IDLE and all stale idle drains up to maxAcquireRetries and throws`() {
        val store = InMemoryPoolStateStore()
        val connectAttempts = AtomicInteger(0)
        // maxIdle=0 keeps the reconcile loop from creating fresh sandboxes against the (missing)
        // server; we drive idle membership manually via putIdle so the test only exercises the
        // acquire retry loop.
        val config =
            PoolConfig.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(0)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .maxAcquireRetries(3)
                .build()
        val pool =
            SandboxPool(
                config = config,
                sandboxManagerFactory = { cfg ->
                    SandboxManager.builder().connectionConfig(cfg).build()
                },
                idleSandboxConnector = { sandboxId ->
                    connectAttempts.incrementAndGet()
                    throw RuntimeException("stale sandbox $sandboxId")
                },
            )
        // 5 stale IDs in idle; retry policy should try exactly 3. The leftover two are excess
        // under maxIdle=0 and the completion-driven reconcile may remove them before the
        // assertion, so the retry budget is verified via connector attempts instead of the
        // residual idle count.
        repeat(5) { store.putIdle("test-pool", "stale-id-$it") }

        pool.start()
        try {
            assertThrows(PoolAcquireFailedException::class.java) {
                pool.acquire(policy = AcquirePolicy.RETRY_NEXT_IDLE)
            }
            assertEquals(3, connectAttempts.get())
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `acquire with RETRY_NEXT_IDLE drained mid-loop still throws PoolAcquireFailedException`() {
        val store = InMemoryPoolStateStore()
        val pool =
            SandboxPool.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(0)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .maxAcquireRetries(5)
                .build()
        // Only 2 stale IDs but budget is 5; loop should exit early after the store empties out
        // and still surface PoolAcquireFailedException (not PoolEmptyException) because at
        // least one candidate was attempted.
        store.putIdle("test-pool", "stale-1")
        store.putIdle("test-pool", "stale-2")

        pool.start()
        try {
            val ex =
                assertThrows(PoolAcquireFailedException::class.java) {
                    pool.acquire(policy = AcquirePolicy.RETRY_NEXT_IDLE)
                }
            assertTrue(ex.message?.contains("drained") == true)
            assertEquals(0, store.snapshotCounters("test-pool").idleCount)
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `acquire with RETRY_NEXT_IDLE_THEN_CREATE falls through to direct create after all idle fail`() {
        val store = InMemoryPoolStateStore()
        val createdSandbox = mockk<Sandbox>(relaxed = true)
        every { createdSandbox.id } returns "created-1"
        val creator = PooledSandboxCreator { createdSandbox }

        val pool =
            SandboxPool.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(0)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(creator)
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .maxAcquireRetries(3)
                .build()
        repeat(3) { store.putIdle("test-pool", "stale-id-$it") }

        pool.start()
        try {
            val sandbox = pool.acquire(policy = AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE)
            assertSame(createdSandbox, sandbox)
            // All three stale entries removed on the way through.
            assertEquals(0, store.snapshotCounters("test-pool").idleCount)
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `cancelled acquire propagates interruption without direct create fallback`() {
        val store = InMemoryPoolStateStore()
        val manager = mockk<SandboxManager>(relaxed = true)
        val connectStarted = CountDownLatch(1)
        val releaseConnect = CountDownLatch(1)
        val acquireExited = CountDownLatch(1)
        val killed = CountDownLatch(1)
        val directCreates = AtomicInteger(0)
        val interruptedAtExit = AtomicBoolean(false)
        val directSandbox = mockk<Sandbox>(relaxed = true)
        every { directSandbox.id } returns "unexpected-direct"
        every { manager.killSandbox("idle-interrupted") } answers { killed.countDown() }
        val connectIdle: (String) -> Sandbox = {
            connectStarted.countDown()
            try {
                releaseConnect.await()
                error("connect should be interrupted")
            } catch (e: InterruptedException) {
                throw RuntimeException("idle readiness interrupted", e)
            }
        }

        val config =
            PoolConfig.builder()
                .poolName("interrupt-pool")
                .ownerId("interrupt-owner")
                .maxIdle(0)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(
                    PooledSandboxCreator {
                        directCreates.incrementAndGet()
                        directSandbox
                    },
                ).drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()
        val pool =
            SandboxPool(
                config = config,
                sandboxManagerFactory = { manager },
                idleSandboxConnector = connectIdle,
            )
        val executor = Executors.newSingleThreadExecutor()

        pool.start()
        store.putIdle("interrupt-pool", "idle-interrupted")
        try {
            val future =
                executor.submit<Sandbox> {
                    try {
                        pool.acquire(policy = AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE)
                    } finally {
                        interruptedAtExit.set(Thread.currentThread().isInterrupted)
                        acquireExited.countDown()
                    }
                }
            assertTrue(connectStarted.await(5, TimeUnit.SECONDS))

            assertTrue(future.cancel(true))
            assertTrue(acquireExited.await(5, TimeUnit.SECONDS))
            assertTrue(killed.await(5, TimeUnit.SECONDS))

            assertEquals(0, directCreates.get(), "cancelled acquire must not fall through to create")
            assertEquals(true, interruptedAtExit.get(), "acquire must restore the worker interrupt status")
            assertEquals(0, store.snapshotCounters("interrupt-pool").idleCount)
        } finally {
            releaseConnect.countDown()
            executor.shutdownNow()
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `run retirement is atomic with taking idle`() {
        val store = BlockingTakePoolStateStore()
        val manager = mockk<SandboxManager>(relaxed = true)
        val newSandbox = mockk<Sandbox>(relaxed = true)
        every { newSandbox.id } returns "new-run-idle"

        val config =
            PoolConfig.builder()
                .poolName("atomic-take-pool")
                .ownerId("atomic-take-owner")
                .maxIdle(0)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()
        val pool =
            SandboxPool(
                config = config,
                sandboxManagerFactory = { manager },
                idleSandboxConnector = { newSandbox },
            )
        val acquireExecutor = Executors.newSingleThreadExecutor()
        val restartExecutor = Executors.newSingleThreadExecutor()
        val restartStarted = CountDownLatch(1)

        pool.start()
        try {
            val oldAcquire =
                acquireExecutor.submit<Sandbox> {
                    pool.acquire(policy = AcquirePolicy.FAIL_FAST)
                }
            assertTrue(store.takeStarted.await(5, TimeUnit.SECONDS))

            val restart =
                restartExecutor.submit {
                    restartStarted.countDown()
                    pool.shutdown(graceful = false)
                    pool.start()
                    store.putIdle("atomic-take-pool", "new-run-idle")
                }
            assertTrue(restartStarted.await(5, TimeUnit.SECONDS))
            assertThrows(TimeoutException::class.java) {
                restart.get(100, TimeUnit.MILLISECONDS)
            }

            store.releaseTake.countDown()
            restart.get(5, TimeUnit.SECONDS)
            assertThrows(ExecutionException::class.java) {
                oldAcquire.get(5, TimeUnit.SECONDS)
            }

            assertEquals(1, store.snapshotCounters("atomic-take-pool").idleCount)
            assertSame(newSandbox, pool.acquire(policy = AcquirePolicy.FAIL_FAST))
            verify(exactly = 0) { manager.killSandbox("new-run-idle") }
        } finally {
            store.releaseTake.countDown()
            acquireExecutor.shutdownNow()
            restartExecutor.shutdownNow()
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `retired acquire cannot consume idle from restarted run`() {
        val store = InMemoryPoolStateStore()
        val manager = mockk<SandboxManager>(relaxed = true)
        val firstConnectStarted = CountDownLatch(1)
        val releaseFirstConnect = CountDownLatch(1)
        val oldSandboxKilled = CountDownLatch(1)
        val connectCalls = AtomicInteger(0)
        val newSandbox = mockk<Sandbox>(relaxed = true)
        every { newSandbox.id } returns "new-run-idle"
        every { manager.killSandbox("old-run-idle") } answers { oldSandboxKilled.countDown() }
        val connectIdle: (String) -> Sandbox = {
            if (connectCalls.incrementAndGet() == 1) {
                firstConnectStarted.countDown()
                releaseFirstConnect.await()
                throw RuntimeException("old candidate failed readiness")
            }
            newSandbox
        }

        val config =
            PoolConfig.builder()
                .poolName("restart-acquire-pool")
                .ownerId("restart-acquire-owner")
                .maxIdle(0)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .maxAcquireRetries(2)
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()
        val pool =
            SandboxPool(
                config = config,
                sandboxManagerFactory = { manager },
                idleSandboxConnector = connectIdle,
            )
        val executor = Executors.newSingleThreadExecutor()

        pool.start()
        store.putIdle("restart-acquire-pool", "old-run-idle")
        try {
            val oldAcquire =
                executor.submit<Sandbox> {
                    pool.acquire(policy = AcquirePolicy.RETRY_NEXT_IDLE)
                }
            assertTrue(firstConnectStarted.await(5, TimeUnit.SECONDS))

            pool.shutdown(graceful = false)
            pool.start()
            store.putIdle("restart-acquire-pool", "new-run-idle")
            releaseFirstConnect.countDown()

            val failure =
                assertThrows(ExecutionException::class.java) {
                    oldAcquire.get(5, TimeUnit.SECONDS)
                }
            assertTrue(failure.cause is PoolNotRunningException)
            assertTrue(oldSandboxKilled.await(5, TimeUnit.SECONDS))
            assertEquals(1, store.snapshotCounters("restart-acquire-pool").idleCount)

            val acquired = pool.acquire(policy = AcquirePolicy.FAIL_FAST)
            assertSame(newSandbox, acquired)
            assertEquals(2, connectCalls.get())
        } finally {
            releaseFirstConnect.countDown()
            executor.shutdownNow()
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `acquire health check Error cleans popped idle before propagating`() {
        val store = InMemoryPoolStateStore()
        val manager = mockk<SandboxManager>(relaxed = true)
        val expected = AssertionError("user health check failed")
        val killed = CountDownLatch(1)
        every { manager.killSandbox("error-idle") } answers { killed.countDown() }
        val connectIdle: (String) -> Sandbox = { throw expected }

        val config =
            PoolConfig.builder()
                .poolName("error-acquire-pool")
                .ownerId("error-acquire-owner")
                .maxIdle(0)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()
        val pool =
            SandboxPool(
                config = config,
                sandboxManagerFactory = { manager },
                idleSandboxConnector = connectIdle,
            )

        pool.start()
        store.putIdle("error-acquire-pool", "error-idle")
        try {
            val actual =
                assertThrows(AssertionError::class.java) {
                    pool.acquire(policy = AcquirePolicy.FAIL_FAST)
                }

            assertSame(expected, actual)
            assertTrue(killed.await(5, TimeUnit.SECONDS))
            assertEquals(0, store.snapshotCounters("error-acquire-pool").idleCount)
            awaitCondition { pool.snapshot().inFlightOperations == 0 }
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `warmup Error cleans sandbox and releases rolling slot without immediate replacement`() {
        val store = InMemoryPoolStateStore()
        val manager = mockk<SandboxManager>(relaxed = true)
        val firstSandbox = mockk<Sandbox>(relaxed = true)
        val created = AtomicInteger(0)
        every { firstSandbox.id } returns "warmup-error"

        val config =
            PoolConfig.builder()
                .poolName("warmup-error-pool")
                .ownerId("warmup-error-owner")
                .maxIdle(1)
                .warmupConcurrency(1)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(
                    PooledSandboxCreator {
                        created.incrementAndGet()
                        firstSandbox
                    },
                ).warmupSkipHealthCheck()
                .warmupSandboxPreparer(
                    SandboxPreparer { sandbox ->
                        if (sandbox.id == "warmup-error") {
                            throw AssertionError("user warmup callback failed")
                        }
                    },
                ).drainTimeout(Duration.ofSeconds(2))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()
        val pool = SandboxPool(config = config, sandboxManagerFactory = { manager })

        pool.start()
        try {
            awaitCondition {
                pool.snapshot().failureCount >= 1 &&
                    pool.snapshot().inFlightOperations == 0 &&
                    currentRunWarming(pool).get() == 0
            }

            assertEquals(1, created.get(), "failed warmup must not be retried before the periodic tick")
            assertEquals(0, store.snapshotCounters("warmup-error-pool").idleCount)
            verify(exactly = 1) { firstSandbox.kill() }
            verify(exactly = 1) { firstSandbox.close() }
        } finally {
            pool.releaseAllIdle()
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `acquire with RETRY_NEXT_IDLE_THEN_CREATE and empty idle falls through immediately`() {
        val store = InMemoryPoolStateStore()
        val createdSandbox = mockk<Sandbox>(relaxed = true)
        every { createdSandbox.id } returns "created-1"
        val creator = PooledSandboxCreator { createdSandbox }

        val pool =
            SandboxPool.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(0)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(creator)
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()

        pool.start()
        try {
            val sandbox = pool.acquire(policy = AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE)
            assertSame(createdSandbox, sandbox)
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `acquire with RETRY_NEXT_IDLE_THEN_CREATE falls through on state store outage`() {
        // Regression: PoolStateStoreUnavailableException during tryTakeIdle must degrade to
        // direct-create under RETRY_NEXT_IDLE_THEN_CREATE (and DIRECT_CREATE), per OSEP-0005.
        // Previously the exception propagated and skipped the fallback branch, making the new
        // then-create policy strictly less available than documented during store outages.
        val createdSandbox = mockk<Sandbox>(relaxed = true)
        every { createdSandbox.id } returns "created-fallback"
        val creator = PooledSandboxCreator { createdSandbox }
        val store = OutageStore()

        val pool =
            SandboxPool.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(0)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(creator)
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()

        pool.start()
        try {
            val sandbox = pool.acquire(policy = AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE)
            assertSame(createdSandbox, sandbox)
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `acquire with RETRY_NEXT_IDLE surfaces state store outage`() {
        // Complement: non-fallthrough policies (FAIL_FAST / RETRY_NEXT_IDLE) must NOT degrade
        // to direct-create on store outage; they must surface the exception so callers can react.
        val store = OutageStore()
        val pool =
            SandboxPool.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(0)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()

        pool.start()
        try {
            assertThrows(
                com.alibaba.opensandbox.sandbox.domain.exceptions.PoolStateStoreUnavailableException::class.java,
            ) {
                pool.acquire(policy = AcquirePolicy.RETRY_NEXT_IDLE)
            }
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `acquire with RETRY_NEXT_IDLE_THEN_CREATE falls through when full state store outage also fails namespace check`() {
        // Regression for Codex round-5 P2: previously, when the full state store was down
        // (Redis outage affecting *all* methods, not just tryTakeIdle), acquire aborted at
        // the pre-loop ensurePoolNamespaceActive call before the fallthrough branch could
        // run. RETRY_NEXT_IDLE_THEN_CREATE is documented to degrade to direct-create during
        // store outages (OSEP-0005); this test proves the namespace check no longer breaks
        // that guarantee.
        val createdSandbox = mockk<Sandbox>(relaxed = true)
        every { createdSandbox.id } returns "created-fallback"
        val creator = PooledSandboxCreator { createdSandbox }
        // Start with getDestroyState working so pool.start() succeeds, then flip to outage
        // mode. This mirrors a real Redis instance that crashes after the pool warms.
        val store = OutageStoreWithNamespaceFailure()
        val pool =
            SandboxPool.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(0)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(creator)
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()

        pool.start()
        store.outage = true
        try {
            val sandbox = pool.acquire(policy = AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE)
            assertSame(createdSandbox, sandbox)
        } finally {
            store.outage = false
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `acquire with RETRY_NEXT_IDLE surfaces full state store outage that also fails namespace check`() {
        // Non-fallthrough counterpart: full state-store outage under RETRY_NEXT_IDLE must
        // still surface PoolStateStoreUnavailableException (fail-closed).
        val store = OutageStoreWithNamespaceFailure()
        val pool =
            SandboxPool.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(0)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()

        pool.start()
        store.outage = true
        try {
            assertThrows(
                com.alibaba.opensandbox.sandbox.domain.exceptions.PoolStateStoreUnavailableException::class.java,
            ) {
                pool.acquire(policy = AcquirePolicy.RETRY_NEXT_IDLE)
            }
        } finally {
            store.outage = false
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `PoolConfig rejects maxAcquireRetries below 1`() {
        val ex =
            assertThrows(IllegalArgumentException::class.java) {
                com.alibaba.opensandbox.sandbox.domain.pool.PoolConfig.builder()
                    .poolName("test-pool")
                    .ownerId("test-owner")
                    .maxIdle(1)
                    .stateStore(InMemoryPoolStateStore())
                    .connectionConfig(ConnectionConfig.builder().build())
                    .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                    .maxAcquireRetries(0)
                    .build()
            }
        assertTrue(ex.message?.contains("maxAcquireRetries") == true)
    }

    @Test
    fun `acquire when pool is stopped throws PoolNotRunningException`() {
        val pool = buildPool()
        assertThrows(PoolNotRunningException::class.java) {
            pool.acquire(policy = AcquirePolicy.DIRECT_CREATE)
        }
    }

    @Test
    fun `releaseAllIdle drains store and returns count`() {
        val store = InMemoryPoolStateStore()
        val pool =
            SandboxPool.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(2)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()
        store.putIdle("test-pool", "id-1")
        store.putIdle("test-pool", "id-2")
        assertEquals(2, store.snapshotCounters("test-pool").idleCount)
        val released = pool.releaseAllIdle()
        assertEquals(2, released)
        assertEquals(0, store.snapshotCounters("test-pool").idleCount)
    }

    @Test
    fun `releaseAllIdle after shutdown uses temporary sandbox manager to kill remote idle sandboxes`() {
        val store = InMemoryPoolStateStore()
        val temporaryManager = mockk<SandboxManager>()
        every { temporaryManager.killSandbox("id-1") } just runs
        every { temporaryManager.killSandbox("id-2") } just runs
        every { temporaryManager.close() } just runs

        val pool =
            SandboxPool(
                config =
                    com.alibaba.opensandbox.sandbox.domain.pool.PoolConfig.builder()
                        .poolName("test-pool")
                        .ownerId("test-owner")
                        .maxIdle(2)
                        .stateStore(store)
                        .connectionConfig(ConnectionConfig.builder().build())
                        .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                        .drainTimeout(Duration.ofMillis(50))
                        .reconcileInterval(Duration.ofSeconds(30))
                        .build(),
                sandboxManagerFactory = { temporaryManager },
            )
        store.putIdle("test-pool", "id-1")
        store.putIdle("test-pool", "id-2")

        val released = pool.releaseAllIdle()

        assertEquals(2, released)
        assertEquals(0, store.snapshotCounters("test-pool").idleCount)
        verify(exactly = 1) { temporaryManager.killSandbox("id-1") }
        verify(exactly = 1) { temporaryManager.killSandbox("id-2") }
        verify(exactly = 1) { temporaryManager.close() }
    }

    @Test
    fun `releaseAllIdle bounds kills and cleans up before store failure`() {
        val delegate = InMemoryPoolStateStore()
        repeat(55) { delegate.putIdle("test-pool", "id-$it") }
        val store =
            object : PoolStateStore by delegate {
                var takes = 0

                override fun tryTakeIdle(poolName: String): String? {
                    if (takes == 55) throw RuntimeException("injected store failure")
                    takes++
                    return delegate.tryTakeIdle(poolName)
                }
            }
        val active = AtomicInteger()
        val maxActive = AtomicInteger()
        val ready = CountDownLatch(50)
        val killed = AtomicInteger()
        val temporaryManager = mockk<SandboxManager>()
        every { temporaryManager.killSandbox(any()) } answers {
            val current = active.incrementAndGet()
            maxActive.updateAndGet { maxOf(it, current) }
            ready.countDown()
            assertTrue(ready.await(2, TimeUnit.SECONDS))
            killed.incrementAndGet()
            active.decrementAndGet()
            if (firstArg<String>() == "id-0") throw RuntimeException("injected kill failure")
        }
        every { temporaryManager.close() } just runs
        val pool =
            SandboxPool(
                config =
                    PoolConfig.builder()
                        .poolName("test-pool")
                        .ownerId("test-owner")
                        .maxIdle(0)
                        .stateStore(store)
                        .connectionConfig(ConnectionConfig.builder().build())
                        .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                        .build(),
                sandboxManagerFactory = { temporaryManager },
            )

        assertThrows(IllegalArgumentException::class.java) { pool.releaseAllIdle(0) }
        val failure = assertThrows(RuntimeException::class.java) { pool.releaseAllIdle(50) }

        assertEquals("injected store failure", failure.message)
        assertEquals(50, maxActive.get())
        assertEquals(55, killed.get())
        assertEquals(0, store.snapshotCounters("test-pool").idleCount)
        verify(exactly = 1) { temporaryManager.close() }
    }

    @Test
    fun `releaseAllIdle waits for kills before closing manager when caller is interrupted`() {
        val store = InMemoryPoolStateStore()
        store.putIdle("test-pool", "id-1")
        val killStarted = CountDownLatch(1)
        val releaseKill = CountDownLatch(1)
        val temporaryManager = mockk<SandboxManager>()
        every { temporaryManager.killSandbox("id-1") } answers {
            killStarted.countDown()
            releaseKill.await()
        }
        every { temporaryManager.close() } just runs
        val pool =
            SandboxPool(
                config =
                    PoolConfig.builder()
                        .poolName("test-pool")
                        .ownerId("test-owner")
                        .maxIdle(0)
                        .stateStore(store)
                        .connectionConfig(ConnectionConfig.builder().build())
                        .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                        .build(),
                sandboxManagerFactory = { temporaryManager },
            )
        val released = AtomicInteger()
        val interruptRestored = AtomicBoolean()
        val caller =
            Thread {
                released.set(pool.releaseAllIdle(1))
                interruptRestored.set(Thread.currentThread().isInterrupted)
            }

        caller.start()
        assertTrue(killStarted.await(2, TimeUnit.SECONDS))
        caller.interrupt()
        Thread.sleep(20)
        assertTrue(caller.isAlive)
        verify(exactly = 0) { temporaryManager.close() }
        releaseKill.countDown()
        caller.join(2_000)

        assertEquals(1, released.get())
        assertTrue(interruptRestored.get())
        verify(exactly = 1) { temporaryManager.close() }
    }

    @Test
    fun `releaseAllIdle drains store even when temporary sandbox manager creation fails`() {
        val store = InMemoryPoolStateStore()
        val pool =
            SandboxPool(
                config =
                    com.alibaba.opensandbox.sandbox.domain.pool.PoolConfig.builder()
                        .poolName("test-pool")
                        .ownerId("test-owner")
                        .maxIdle(2)
                        .stateStore(store)
                        .connectionConfig(ConnectionConfig.builder().build())
                        .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                        .drainTimeout(Duration.ofMillis(50))
                        .reconcileInterval(Duration.ofSeconds(30))
                        .build(),
                sandboxManagerFactory = { throw RuntimeException("manager init failed") },
            )
        store.putIdle("test-pool", "id-1")
        store.putIdle("test-pool", "id-2")

        val released = pool.releaseAllIdle()

        assertEquals(2, released)
        assertEquals(0, store.snapshotCounters("test-pool").idleCount)
    }

    @Test
    fun `shutdown non-graceful force stops executors when await timeout`() {
        val pool = buildPool()
        val reconcileTask = mockk<ScheduledFuture<*>>()
        val scheduler = mockk<ScheduledExecutorService>()
        val warmup = mockk<ExecutorService>()

        every { reconcileTask.cancel(true) } returns true

        every { scheduler.shutdown() } just runs
        every { scheduler.awaitTermination(5, TimeUnit.SECONDS) } returnsMany listOf(false, true)
        every { scheduler.shutdownNow() } returns emptyList()

        every { warmup.shutdown() } just runs
        every { warmup.awaitTermination(5, TimeUnit.SECONDS) } returnsMany listOf(false, true)
        every { warmup.shutdownNow() } returns emptyList()

        setPrivateField(pool, "reconcileTask", reconcileTask)
        setPrivateField(pool, "scheduler", scheduler)
        setPrivateField(pool, "warmupExecutor", warmup)

        pool.shutdown(graceful = false)

        verify(exactly = 1) { reconcileTask.cancel(true) }
        verify(exactly = 1) { scheduler.shutdown() }
        verify(exactly = 1) { scheduler.shutdownNow() }
        verify(exactly = 2) { scheduler.awaitTermination(5, TimeUnit.SECONDS) }
        verify(exactly = 1) { warmup.shutdown() }
        verify(exactly = 1) { warmup.shutdownNow() }
        verify(exactly = 2) { warmup.awaitTermination(5, TimeUnit.SECONDS) }
    }

    @Test
    fun `shutdown non-graceful does not force stop executors when await succeeds`() {
        val pool = buildPool()
        val reconcileTask = mockk<ScheduledFuture<*>>()
        val scheduler = mockk<ScheduledExecutorService>()
        val warmup = mockk<ExecutorService>()

        every { reconcileTask.cancel(true) } returns true
        every { scheduler.shutdown() } just runs
        every { scheduler.awaitTermination(5, TimeUnit.SECONDS) } returns true
        every { scheduler.shutdownNow() } returns emptyList()
        every { warmup.shutdown() } just runs
        every { warmup.awaitTermination(5, TimeUnit.SECONDS) } returns true
        every { warmup.shutdownNow() } returns emptyList()

        setPrivateField(pool, "reconcileTask", reconcileTask)
        setPrivateField(pool, "scheduler", scheduler)
        setPrivateField(pool, "warmupExecutor", warmup)

        pool.shutdown(graceful = false)

        verify(exactly = 0) { scheduler.shutdownNow() }
        verify(exactly = 0) { warmup.shutdownNow() }
    }

    @Test
    fun `pool creation spec builder keeps extensions`() {
        val spec =
            PoolCreationSpec.builder()
                .image("ubuntu:22.04")
                .extension("storage.id", "abc123")
                .extensions(mapOf("debug" to "true"))
                .build()

        assertEquals("abc123", spec.extensions["storage.id"])
        assertEquals("true", spec.extensions["debug"])
    }

    @Test
    fun `applyToBuilder propagates pool creation spec extensions to sandbox builder`() {
        val spec =
            PoolCreationSpec.builder()
                .image("ubuntu:22.04")
                .env(mapOf("ENV_1" to "value"))
                .metadata(mapOf("meta" to "data"))
                .extensions(mapOf("storage.id" to "abc123", "debug" to "true"))
                .build()

        val builder = spec.applyToBuilder(Sandbox.builder())

        val extensionsField = builder.javaClass.getDeclaredField("extensions")
        extensionsField.isAccessible = true
        @Suppress("UNCHECKED_CAST")
        val extensions = extensionsField.get(builder) as MutableMap<String, String>
        assertEquals("abc123", extensions["storage.id"])
        assertEquals("true", extensions["debug"])
    }

    @Test
    fun `applyToBuilder propagates pool creation spec platform to sandbox builder`() {
        val platform =
            PlatformSpec.builder()
                .os("linux")
                .arch("arm64")
                .build()
        val spec =
            PoolCreationSpec.builder()
                .image("ubuntu:22.04")
                .platform(platform)
                .build()

        val builder = spec.applyToBuilder(Sandbox.builder())

        val platformField = builder.javaClass.getDeclaredField("platform")
        platformField.isAccessible = true
        assertSame(platform, platformField.get(builder))
    }

    @Test
    fun `applyToBuilder propagates pool creation spec credential proxy to sandbox builder`() {
        val credentialProxy = CredentialProxyConfig.enabled()
        val spec =
            PoolCreationSpec.builder()
                .image("ubuntu:22.04")
                .credentialProxy(credentialProxy)
                .build()

        val builder = spec.applyToBuilder(Sandbox.builder())

        val credentialProxyField = builder.javaClass.getDeclaredField("credentialProxy")
        credentialProxyField.isAccessible = true
        assertSame(credentialProxy, credentialProxyField.get(builder))
    }

    @Test
    fun `pool creation spec builder convenience methods align with sandbox builder semantics`() {
        val volume =
            Volume.builder()
                .name("data")
                .host(Host.of("/tmp/data"))
                .mountPath("/data")
                .readOnly(false)
                .build()

        val spec =
            PoolCreationSpec.builder()
                .image("ubuntu:22.04")
                .env("ENV_1", "value-1")
                .env { put("ENV_2", "value-2") }
                .metadata("meta-1", "value-1")
                .metadata { put("meta-2", "value-2") }
                .secureAccess()
                .networkPolicy {
                    defaultAction(NetworkPolicy.DefaultAction.DENY)
                    addEgress(
                        NetworkRule.builder()
                            .action(NetworkRule.Action.ALLOW)
                            .target("pypi.org")
                            .build(),
                    )
                }
                .volume(volume)
                .volume {
                    name("cache")
                    host(Host.of("/tmp/cache"))
                    mountPath("/cache")
                    readOnly(true)
                }
                .build()

        assertEquals("value-1", spec.env["ENV_1"])
        assertEquals("value-2", spec.env["ENV_2"])
        assertEquals("value-1", spec.metadata["meta-1"])
        assertEquals("value-2", spec.metadata["meta-2"])
        assertEquals(true, spec.secureAccess)
        assertEquals(NetworkPolicy.DefaultAction.DENY, spec.networkPolicy?.defaultAction)
        assertEquals("pypi.org", spec.networkPolicy?.egress?.firstOrNull()?.target)
        assertEquals(2, spec.volumes?.size)
        assertEquals("/data", spec.volumes?.get(0)?.mountPath)
        assertEquals("/cache", spec.volumes?.get(1)?.mountPath)
    }

    @Test
    fun `sandbox pool builder forwards warmup readiness settings into config`() {
        val healthCheck: (Sandbox) -> Boolean = { true }
        val preparer = SandboxPreparer {}
        val pool =
            SandboxPool.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(2)
                .stateStore(InMemoryPoolStateStore())
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .warmupReadyTimeout(Duration.ofSeconds(45))
                .warmupHealthCheckPollingInterval(Duration.ofMillis(500))
                .warmupHealthCheck(healthCheck)
                .warmupSandboxPreparer(preparer)
                .warmupSkipHealthCheck()
                .build()

        val configField = pool.javaClass.getDeclaredField("config")
        configField.isAccessible = true
        val config = configField.get(pool) as com.alibaba.opensandbox.sandbox.domain.pool.PoolConfig

        assertEquals(Duration.ofSeconds(45), config.warmupReadyTimeout)
        assertEquals(Duration.ofMillis(500), config.warmupHealthCheckPollingInterval)
        assertSame(healthCheck, config.warmupHealthCheck)
        assertSame(preparer, config.warmupSandboxPreparer)
        assertEquals(true, config.warmupSkipHealthCheck)
    }

    @Test
    fun `sandbox pool builder forwards acquire readiness settings into config`() {
        val healthCheck: (Sandbox) -> Boolean = { true }
        val sandboxCreator = PooledSandboxCreator { mockk<Sandbox>() }
        val pool =
            SandboxPool.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(2)
                .stateStore(InMemoryPoolStateStore())
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .acquireReadyTimeout(Duration.ofSeconds(5))
                .acquireHealthCheckPollingInterval(Duration.ofMillis(50))
                .acquireHealthCheck(healthCheck)
                .acquireSkipHealthCheck()
                .acquireMinRemainingTtl(Duration.ofSeconds(90))
                .sandboxCreator(sandboxCreator)
                .idleTimeout(Duration.ofMinutes(15))
                .build()

        val configField = pool.javaClass.getDeclaredField("config")
        configField.isAccessible = true
        val config = configField.get(pool) as com.alibaba.opensandbox.sandbox.domain.pool.PoolConfig

        assertEquals(Duration.ofSeconds(5), config.acquireReadyTimeout)
        assertEquals(Duration.ofMillis(50), config.acquireHealthCheckPollingInterval)
        assertSame(healthCheck, config.acquireHealthCheck)
        assertEquals(true, config.acquireSkipHealthCheck)
        assertEquals(Duration.ofSeconds(90), config.acquireMinRemainingTtl)
        assertSame(sandboxCreator, config.sandboxCreator)
        assertEquals(Duration.ofMinutes(15), config.idleTimeout)
    }

    @Test
    fun `start aligns state store idle ttl hook with idleTimeout`() {
        val store = InMemoryPoolStateStore()
        val pool =
            SandboxPool.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(2)
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .idleTimeout(Duration.ofMinutes(10))
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()

        pool.start()
        try {
            store.putIdle("test-pool", "id-1")
            store.reapExpiredIdle("test-pool", java.time.Instant.now().plus(Duration.ofMinutes(11)))
            assertEquals(0, store.snapshotCounters("test-pool").idleCount)
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `start overwrites shared maxIdle with user config`() {
        val store = RecordingPoolStateStore(initialMaxIdle = 0)
        val pool = buildPool(store = store, maxIdle = 3)

        pool.start()
        try {
            assertEquals(3, store.maxIdleByPool["test-pool"])
            assertEquals(listOf("test-pool" to 3), store.setMaxIdleCalls)
            assertEquals(3, pool.snapshot().maxIdle)
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `custom direct create kills and closes when renew fails`() {
        val sandbox = mockk<Sandbox>()
        every { sandbox.renew(Duration.ofMinutes(5)) } throws RuntimeException("renew failed")
        every { sandbox.kill() } just runs
        every { sandbox.close() } just runs

        val pool =
            SandboxPool.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(0)
                .stateStore(InMemoryPoolStateStore())
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .sandboxCreator(PooledSandboxCreator { sandbox })
                .drainTimeout(Duration.ofMillis(50))
                .reconcileInterval(Duration.ofSeconds(30))
                .build()
        pool.start()

        try {
            assertThrows(RuntimeException::class.java) {
                pool.acquire(Duration.ofMinutes(5))
            }

            verify(exactly = 1) { sandbox.kill() }
            verify(exactly = 1) { sandbox.close() }
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    private fun buildPool(
        store: PoolStateStore = InMemoryPoolStateStore(),
        maxIdle: Int = 2,
    ): SandboxPool {
        val config = ConnectionConfig.builder().build()
        val spec = PoolCreationSpec.builder().image("ubuntu:22.04").build()
        return SandboxPool.builder()
            .poolName("test-pool")
            .ownerId("test-owner")
            .maxIdle(maxIdle)
            .stateStore(store)
            .connectionConfig(config)
            .creationSpec(spec)
            .drainTimeout(Duration.ofMillis(50))
            .reconcileInterval(Duration.ofSeconds(30))
            .build()
    }

    private fun buildDiscardedCleanupPool(
        store: PoolStateStore,
        manager: SandboxManager,
        directSandbox: Sandbox,
        drainTimeout: Duration,
    ): SandboxPool =
        SandboxPool(
            config =
                PoolConfig.builder()
                    .poolName("test-pool")
                    .ownerId("test-owner")
                    .maxIdle(0)
                    .stateStore(store)
                    .connectionConfig(ConnectionConfig.builder().build())
                    .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                    .sandboxCreator(PooledSandboxCreator { directSandbox })
                    .acquireMinRemainingTtl(Duration.ofMinutes(1))
                    .idleTimeout(Duration.ofMinutes(10))
                    .drainTimeout(drainTimeout)
                    .reconcileInterval(Duration.ofSeconds(30))
                    .build(),
            sandboxManagerFactory = { manager },
        )

    private class DiscardingPoolStateStore : PoolStateStore by InMemoryPoolStateStore() {
        override fun tryTakeIdle(
            poolName: String,
            minRemainingTtl: Duration,
        ): TakeIdleResult =
            TakeIdleResult(
                sandboxId = null,
                discardedAliveSandboxIds = listOf("near-expiry-id"),
            )
    }

    private class CountingPoolStateStore(
        private val delegate: InMemoryPoolStateStore = InMemoryPoolStateStore(),
    ) : PoolStateStore by delegate {
        /** Number of reconcile ticks, proxied by the primary-lock acquisition that opens each tick. */
        val reconcileTicks = AtomicInteger(0)

        override fun tryAcquirePrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean {
            reconcileTicks.incrementAndGet()
            return delegate.tryAcquirePrimaryLock(poolName, ownerId, ttl)
        }
    }

    private class BlockingTakePoolStateStore(
        private val delegate: InMemoryPoolStateStore = InMemoryPoolStateStore(),
    ) : PoolStateStore by delegate {
        val takeStarted = CountDownLatch(1)
        val releaseTake = CountDownLatch(1)

        override fun tryTakeIdle(
            poolName: String,
            minRemainingTtl: Duration,
        ): TakeIdleResult {
            takeStarted.countDown()
            releaseTake.await()
            return delegate.tryTakeIdle(poolName, minRemainingTtl)
        }
    }

    @Suppress("UNCHECKED_CAST")
    private fun <T> getPrivateField(
        target: Any,
        fieldName: String,
    ): T {
        val field = target.javaClass.getDeclaredField(fieldName)
        field.isAccessible = true
        return field.get(target) as T
    }

    private fun setPrivateField(
        target: Any,
        fieldName: String,
        value: Any?,
    ) {
        val field = target.javaClass.getDeclaredField(fieldName)
        field.isAccessible = true
        field.set(target, value)
    }

    private fun currentRunInFlight(pool: SandboxPool): AtomicInteger {
        val run = getPrivateField<Any>(pool, "currentRun")
        return getPrivateField(run, "inFlightOperations")
    }

    private fun currentRunWarming(pool: SandboxPool): AtomicInteger {
        val run = getPrivateField<Any>(pool, "currentRun")
        return getPrivateField(run, "warmingCount")
    }

    private fun currentRunPendingWarmupCompletions(pool: SandboxPool): Set<*> {
        val run = getPrivateField<Any>(pool, "currentRun")
        return getPrivateField(run, "pendingWarmupCompletions")
    }

    private fun awaitCondition(
        timeout: Duration = Duration.ofSeconds(5),
        condition: () -> Boolean,
    ) {
        val deadline = System.nanoTime() + timeout.toNanos()
        while (!condition()) {
            if (System.nanoTime() >= deadline) {
                throw AssertionError("Condition was not satisfied within $timeout")
            }
            Thread.sleep(10)
        }
    }

    private fun awaitIgnoringInterrupt(latch: CountDownLatch) {
        while (true) {
            try {
                latch.await()
                return
            } catch (_: InterruptedException) {
                // Simulates an external preparer that does not cooperate with executor cancellation.
            }
        }
    }

    private fun shutdownWithoutWaitingForUncooperativeWorker(pool: SandboxPool) {
        Thread.currentThread().interrupt()
        try {
            pool.shutdown(graceful = false)
        } finally {
            Thread.interrupted()
        }
    }

    private class HeartbeatRecordingStore : PoolStateStore by InMemoryPoolStateStore() {
        val renewCalls = AtomicInteger(0)

        override fun renewPrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean {
            renewCalls.incrementAndGet()
            return true
        }
    }

    private class RenewFailsAfterAdmissionStore : PoolStateStore by InMemoryPoolStateStore() {
        private val renewCalls = AtomicInteger(0)

        override fun renewPrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean = renewCalls.incrementAndGet() == 1
    }

    private class PutIdleFailureStore : PoolStateStore by InMemoryPoolStateStore() {
        override fun putIdle(
            poolName: String,
            sandboxId: String,
        ) {
            throw RuntimeException("put failed")
        }
    }

    /**
     * Store that raises [PoolStateStoreUnavailableException] from every take call. Used to
     * exercise the state-store-outage fallback path in acquire.
     */
    private class OutageStore : PoolStateStore {
        override fun tryTakeIdle(poolName: String): String? {
            throw com.alibaba.opensandbox.sandbox.domain.exceptions.PoolStateStoreUnavailableException(
                "tryTakeIdle",
                RuntimeException("redis unavailable"),
            )
        }

        override fun tryTakeIdle(
            poolName: String,
            minRemainingTtl: Duration,
        ): com.alibaba.opensandbox.sandbox.domain.pool.TakeIdleResult {
            throw com.alibaba.opensandbox.sandbox.domain.exceptions.PoolStateStoreUnavailableException(
                "tryTakeIdleWithMinTtl",
                RuntimeException("redis unavailable"),
            )
        }

        override fun putIdle(
            poolName: String,
            sandboxId: String,
        ) {}

        override fun removeIdle(
            poolName: String,
            sandboxId: String,
        ) {}

        override fun tryAcquirePrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean = true

        override fun renewPrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean = true

        override fun releasePrimaryLock(
            poolName: String,
            ownerId: String,
        ) {}

        override fun reapExpiredIdle(
            poolName: String,
            now: Instant,
        ) {}

        override fun snapshotCounters(poolName: String): StoreCounters = StoreCounters(idleCount = 0)

        override fun snapshotIdleEntries(poolName: String): List<IdleEntry> = emptyList()

        override fun getMaxIdle(poolName: String): Int? = null

        override fun setMaxIdle(
            poolName: String,
            maxIdle: Int,
        ) {}
    }

    /**
     * Store that starts healthy, then flips to full outage on demand — every method
     * (including [getDestroyState]) raises [PoolStateStoreUnavailableException]. Used
     * to exercise the Codex round-5 regression where the namespace-check on the acquire
     * path aborted before the fallthrough branch could run.
     */
    private class OutageStoreWithNamespaceFailure : PoolStateStore {
        @Volatile var outage: Boolean = false

        private fun bang(op: String): Nothing =
            throw com.alibaba.opensandbox.sandbox.domain.exceptions.PoolStateStoreUnavailableException(
                op,
                RuntimeException("redis unavailable"),
            )

        override fun tryTakeIdle(poolName: String): String? {
            if (outage) bang("tryTakeIdle")
            return null
        }

        override fun tryTakeIdle(
            poolName: String,
            minRemainingTtl: Duration,
        ): com.alibaba.opensandbox.sandbox.domain.pool.TakeIdleResult {
            if (outage) bang("tryTakeIdleWithMinTtl")
            return com.alibaba.opensandbox.sandbox.domain.pool.TakeIdleResult.of(null)
        }

        override fun putIdle(
            poolName: String,
            sandboxId: String,
        ) {
            if (outage) bang("putIdle")
        }

        override fun removeIdle(
            poolName: String,
            sandboxId: String,
        ) {
            if (outage) bang("removeIdle")
        }

        override fun tryAcquirePrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean {
            if (outage) bang("tryAcquirePrimaryLock")
            return true
        }

        override fun renewPrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean {
            if (outage) bang("renewPrimaryLock")
            return true
        }

        override fun releasePrimaryLock(
            poolName: String,
            ownerId: String,
        ) {
            if (outage) bang("releasePrimaryLock")
        }

        override fun reapExpiredIdle(
            poolName: String,
            now: Instant,
        ) {
            if (outage) bang("reapExpiredIdle")
        }

        override fun snapshotCounters(poolName: String): StoreCounters {
            if (outage) bang("snapshotCounters")
            return StoreCounters(idleCount = 0)
        }

        override fun snapshotIdleEntries(poolName: String): List<IdleEntry> {
            if (outage) bang("snapshotIdleEntries")
            return emptyList()
        }

        override fun getMaxIdle(poolName: String): Int? {
            if (outage) bang("getMaxIdle")
            return null
        }

        override fun setMaxIdle(
            poolName: String,
            maxIdle: Int,
        ) {
            if (outage) bang("setMaxIdle")
        }

        override fun getDestroyState(poolName: String): PoolDestroyState {
            if (outage) bang("getDestroyState")
            return PoolDestroyState.ACTIVE
        }
    }

    private class RecordingPoolStateStore(
        private val releaseFails: Boolean = false,
        initialMaxIdle: Int? = null,
    ) : PoolStateStore {
        val releasedLocks = mutableListOf<Pair<String, String>>()
        val setMaxIdleCalls = mutableListOf<Pair<String, Int>>()
        val maxIdleByPool = mutableMapOf<String, Int>()

        init {
            if (initialMaxIdle != null) {
                maxIdleByPool["test-pool"] = initialMaxIdle
            }
        }

        override fun tryTakeIdle(poolName: String): String? = null

        override fun putIdle(
            poolName: String,
            sandboxId: String,
        ) {
        }

        override fun removeIdle(
            poolName: String,
            sandboxId: String,
        ) {
        }

        override fun tryAcquirePrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean = true

        override fun renewPrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean = true

        override fun releasePrimaryLock(
            poolName: String,
            ownerId: String,
        ) {
            releasedLocks += poolName to ownerId
            if (releaseFails) {
                throw RuntimeException("release failed")
            }
        }

        override fun reapExpiredIdle(
            poolName: String,
            now: Instant,
        ) {
        }

        override fun snapshotCounters(poolName: String): StoreCounters = StoreCounters(idleCount = 0)

        override fun snapshotIdleEntries(poolName: String): List<IdleEntry> = emptyList()

        override fun getMaxIdle(poolName: String): Int? = maxIdleByPool[poolName]

        override fun setMaxIdle(
            poolName: String,
            maxIdle: Int,
        ) {
            setMaxIdleCalls += poolName to maxIdle
            maxIdleByPool[poolName] = maxIdle
        }
    }
}
