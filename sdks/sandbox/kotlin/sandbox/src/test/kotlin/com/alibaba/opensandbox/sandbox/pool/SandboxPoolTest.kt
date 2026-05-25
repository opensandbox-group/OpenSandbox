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
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Host
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PlatformSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Volume
import com.alibaba.opensandbox.sandbox.domain.pool.AcquirePolicy
import com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec
import com.alibaba.opensandbox.sandbox.domain.pool.PoolLifecycleState
import com.alibaba.opensandbox.sandbox.domain.pool.PoolState
import com.alibaba.opensandbox.sandbox.domain.pool.PoolStateStore
import com.alibaba.opensandbox.sandbox.domain.pool.SandboxPreparer
import com.alibaba.opensandbox.sandbox.domain.pool.StoreCounters
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
import java.time.Duration
import java.time.Instant
import java.util.concurrent.ExecutorService
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger

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
        val inFlight = AtomicInteger(3)
        setPrivateField(pool, "inFlightOperations", inFlight)

        val snap = pool.snapshot()

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
                .idleTimeout(Duration.ofMinutes(15))
                .build()

        val configField = pool.javaClass.getDeclaredField("config")
        configField.isAccessible = true
        val config = configField.get(pool) as com.alibaba.opensandbox.sandbox.domain.pool.PoolConfig

        assertEquals(Duration.ofSeconds(5), config.acquireReadyTimeout)
        assertEquals(Duration.ofMillis(50), config.acquireHealthCheckPollingInterval)
        assertSame(healthCheck, config.acquireHealthCheck)
        assertEquals(true, config.acquireSkipHealthCheck)
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

    private fun setPrivateField(
        target: Any,
        fieldName: String,
        value: Any?,
    ) {
        val field = target.javaClass.getDeclaredField(fieldName)
        field.isAccessible = true
        field.set(target, value)
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
