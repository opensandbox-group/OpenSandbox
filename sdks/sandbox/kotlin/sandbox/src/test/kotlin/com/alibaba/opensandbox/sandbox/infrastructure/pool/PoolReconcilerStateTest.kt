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

package com.alibaba.opensandbox.sandbox.infrastructure.pool

import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.pool.PoolConfig
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec
import com.alibaba.opensandbox.sandbox.domain.pool.PoolState
import com.alibaba.opensandbox.sandbox.domain.pool.PoolStateStore
import com.alibaba.opensandbox.sandbox.domain.pool.StoreCounters
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Test
import java.time.Duration
import java.time.Instant
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicInteger

class PoolReconcilerStateTest {
    @Test
    fun `recordFailure transitions to DEGRADED when failure count reaches threshold`() {
        val state = ReconcileState(degradedThreshold = 3, backoffBase = Duration.ofMillis(10), backoffMax = Duration.ofSeconds(1))
        state.recordFailure("boom-1")
        state.recordFailure("boom-2")
        assertEquals(PoolState.HEALTHY, state.state)
        assertFalse(state.isBackoffActive())

        state.recordFailure("boom-3")
        assertEquals(PoolState.DEGRADED, state.state)
        assertEquals(3, state.failureCount)
    }

    @Test
    fun `default degraded backoff caps at one day`() {
        val state = ReconcileState(degradedThreshold = 1)

        repeat(20) { state.recordFailure("boom") }

        assertEquals(PoolState.DEGRADED, state.state)
        assertEquals(20, state.failureCount)
        assertEquals(true, state.isBackoffActive(Instant.now().plus(Duration.ofHours(23))))
        assertFalse(state.isBackoffActive(Instant.now().plus(Duration.ofHours(25))))
    }

    @Test
    fun `default degraded backoff starts at thirty seconds`() {
        val state = ReconcileState(degradedThreshold = 1)

        state.recordFailure("boom")

        assertEquals(true, state.isBackoffActive(Instant.now().plus(Duration.ofSeconds(29))))
        assertFalse(state.isBackoffActive(Instant.now().plus(Duration.ofSeconds(31))))
    }

    @Test
    fun `reconcile batch failures only advance backoff once`() {
        val stateStore = InMemoryPoolStateStore()
        val config =
            PoolConfig.builder()
                .poolName("pool-batch-failure-test")
                .ownerId("owner-1")
                .maxIdle(10)
                .warmupConcurrency(10)
                .stateStore(stateStore)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .build()
        val state = ReconcileState(degradedThreshold = 3)
        val warmupExecutor = Executors.newFixedThreadPool(10)

        try {
            PoolReconciler.runReconcileTick(
                config = config,
                stateStore = stateStore,
                createOne = { throw RuntimeException("boom") },
                reconcileState = state,
                warmupExecutor = warmupExecutor,
            )
        } finally {
            warmupExecutor.shutdownNow()
        }

        assertEquals(10, state.failureCount)
        assertEquals(true, state.isBackoffActive(Instant.now().plus(Duration.ofSeconds(29))))
        assertFalse(state.isBackoffActive(Instant.now().plus(Duration.ofSeconds(31))))
    }

    @Test
    fun `reconcile create exception increments failure count once per task`() {
        val stateStore = InMemoryPoolStateStore()
        val config =
            PoolConfig.builder()
                .poolName("pool-reconcile-test")
                .ownerId("owner-1")
                .maxIdle(1)
                .warmupConcurrency(1)
                .stateStore(stateStore)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .build()
        val state = ReconcileState(degradedThreshold = 10)
        val warmupExecutor = Executors.newFixedThreadPool(1)

        try {
            PoolReconciler.runReconcileTick(
                config = config,
                stateStore = stateStore,
                createOne = { throw RuntimeException("boom") },
                reconcileState = state,
                warmupExecutor = warmupExecutor,
            )
        } finally {
            warmupExecutor.shutdownNow()
        }

        assertEquals(1, state.failureCount)
    }

    @Test
    fun `reconcile stops putIdle and cleans orphaned sandboxes after lock renew failure`() {
        val stateStore = RenewFailAfterSecondPutStore()
        val config =
            PoolConfig.builder()
                .poolName("pool-lock-window-test")
                .ownerId("owner-1")
                .maxIdle(2)
                .warmupConcurrency(2)
                .stateStore(stateStore)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .build()
        val state = ReconcileState(degradedThreshold = 10)
        val warmupExecutor = Executors.newFixedThreadPool(2)
        val idGen = AtomicInteger(0)
        val orphaned = mutableListOf<String>()

        try {
            PoolReconciler.runReconcileTick(
                config = config,
                stateStore = stateStore,
                createOne = { "id-${idGen.incrementAndGet()}" },
                onDiscardSandbox = { orphaned += it },
                reconcileState = state,
                warmupExecutor = warmupExecutor,
            )
        } finally {
            warmupExecutor.shutdownNow()
        }

        assertEquals(1, stateStore.putIdleIds.size)
        assertEquals(1, orphaned.size)
        val idle = stateStore.putIdleIds.first()
        val dropped = orphaned.first()
        assertFalse(idle == dropped)
        assertEquals(setOf("id-1", "id-2"), setOf(idle, dropped))
    }

    @Test
    fun `reconcile putIdle failure records failure and cleans remaining created sandboxes`() {
        val stateStore = PutIdleFailStore()
        val config =
            PoolConfig.builder()
                .poolName("pool-put-failure-test")
                .ownerId("owner-1")
                .maxIdle(2)
                .warmupConcurrency(2)
                .stateStore(stateStore)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .build()
        val state = ReconcileState(degradedThreshold = 10)
        val warmupExecutor = Executors.newFixedThreadPool(2)
        val idGen = AtomicInteger(0)
        val orphaned = mutableListOf<String>()

        try {
            PoolReconciler.runReconcileTick(
                config = config,
                stateStore = stateStore,
                createOne = { "id-${idGen.incrementAndGet()}" },
                onDiscardSandbox = { orphaned += it },
                reconcileState = state,
                warmupExecutor = warmupExecutor,
            )
        } finally {
            warmupExecutor.shutdownNow()
        }

        assertEquals(1, state.failureCount)
        assertEquals(2, orphaned.size)
        assertEquals(setOf("id-1", "id-2"), orphaned.toSet())
    }

    @Test
    fun `reconcile skips create when current node is not primary`() {
        val stateStore = AlwaysSecondaryStore()
        val config =
            PoolConfig.builder()
                .poolName("pool-not-primary")
                .ownerId("owner-2")
                .maxIdle(1)
                .warmupConcurrency(1)
                .stateStore(stateStore)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .build()
        val state = ReconcileState(degradedThreshold = 10)
        val warmupExecutor = Executors.newFixedThreadPool(1)
        val createCalls = AtomicInteger(0)

        try {
            PoolReconciler.runReconcileTick(
                config = config,
                stateStore = stateStore,
                createOne = {
                    createCalls.incrementAndGet()
                    "id-1"
                },
                reconcileState = state,
                warmupExecutor = warmupExecutor,
            )
        } finally {
            warmupExecutor.shutdownNow()
        }

        assertEquals(0, createCalls.get())
        assertEquals(emptyList<String>(), stateStore.putIdleIds)
    }

    @Test
    fun `only primary owner can reconcile create for same pool`() {
        val stateStore = OwnerLockingStore()
        val primaryConfig = buildConfig(ownerId = "owner-primary", maxIdle = 1, stateStore = stateStore, poolName = "pool-owner-lock")
        val secondaryConfig = buildConfig(ownerId = "owner-secondary", maxIdle = 1, stateStore = stateStore, poolName = "pool-owner-lock")
        val state = ReconcileState(degradedThreshold = 10)
        val warmupExecutor = Executors.newFixedThreadPool(1)
        val secondaryCreateCalls = AtomicInteger(0)

        try {
            PoolReconciler.runReconcileTick(
                config = primaryConfig,
                stateStore = stateStore,
                createOne = { "id-primary-1" },
                reconcileState = state,
                warmupExecutor = warmupExecutor,
            )
            PoolReconciler.runReconcileTick(
                config = secondaryConfig,
                stateStore = stateStore,
                createOne = {
                    secondaryCreateCalls.incrementAndGet()
                    "id-secondary-1"
                },
                reconcileState = state,
                warmupExecutor = warmupExecutor,
            )
        } finally {
            warmupExecutor.shutdownNow()
        }

        assertEquals(0, secondaryCreateCalls.get())
        assertEquals(listOf("id-primary-1"), stateStore.putIdleIds)
    }

    @Test
    fun `reconcile does not create when initial renew fails`() {
        val stateStore = RenewFailsOnFirstCallStore()
        val config = buildConfig(ownerId = "owner-1", maxIdle = 1, stateStore = stateStore, poolName = "pool-renew-first-fail")
        val state = ReconcileState(degradedThreshold = 10)
        val warmupExecutor = Executors.newFixedThreadPool(1)
        val createCalls = AtomicInteger(0)

        try {
            PoolReconciler.runReconcileTick(
                config = config,
                stateStore = stateStore,
                createOne = {
                    createCalls.incrementAndGet()
                    "id-1"
                },
                reconcileState = state,
                warmupExecutor = warmupExecutor,
            )
        } finally {
            warmupExecutor.shutdownNow()
        }

        assertEquals(0, createCalls.get())
        assertEquals(emptyList<String>(), stateStore.putIdleIds)
    }

    @Test
    fun `reconcile shrinks excess idle when current node is primary`() {
        val stateStore = ShrinkStore(idleIds = listOf("id-1", "id-2", "id-3", "id-4", "id-5"))
        val config =
            PoolConfig.builder()
                .poolName("pool-shrink")
                .ownerId("owner-1")
                .maxIdle(2)
                .warmupConcurrency(2)
                .stateStore(stateStore)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .build()
        val state = ReconcileState(degradedThreshold = 10)
        val warmupExecutor = Executors.newFixedThreadPool(1)
        val discarded = mutableListOf<String>()
        val createCalls = AtomicInteger(0)

        try {
            PoolReconciler.runReconcileTick(
                config = config,
                stateStore = stateStore,
                createOne = {
                    createCalls.incrementAndGet()
                    "new-id"
                },
                onDiscardSandbox = { discarded += it },
                reconcileState = state,
                warmupExecutor = warmupExecutor,
            )
        } finally {
            warmupExecutor.shutdownNow()
        }

        assertEquals(0, createCalls.get())
        assertEquals(listOf("id-1", "id-2"), discarded)
        assertEquals(listOf("id-3", "id-4", "id-5"), stateStore.idleIds)
    }

    @Test
    fun `reconcile does not shrink when current node is not primary`() {
        val stateStore = SecondaryShrinkStore(idleIds = listOf("id-1", "id-2", "id-3"))
        val config = buildConfig(ownerId = "owner-2", maxIdle = 1, stateStore = stateStore, poolName = "pool-secondary-shrink")
        val state = ReconcileState(degradedThreshold = 10)
        val warmupExecutor = Executors.newFixedThreadPool(1)
        val discarded = mutableListOf<String>()

        try {
            PoolReconciler.runReconcileTick(
                config = config,
                stateStore = stateStore,
                createOne = { "new-id" },
                onDiscardSandbox = { discarded += it },
                reconcileState = state,
                warmupExecutor = warmupExecutor,
            )
        } finally {
            warmupExecutor.shutdownNow()
        }

        assertEquals(emptyList<String>(), discarded)
        assertEquals(listOf("id-1", "id-2", "id-3"), stateStore.idleIds)
    }

    @Test
    fun `reconcile stops shrinking when primary renew fails`() {
        val stateStore = ShrinkStore(idleIds = listOf("id-1", "id-2", "id-3"), renewFailuresAfter = 1)
        val config =
            PoolConfig.builder()
                .poolName("pool-shrink-renew-fails")
                .ownerId("owner-1")
                .maxIdle(0)
                .warmupConcurrency(3)
                .stateStore(stateStore)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .build()
        val state = ReconcileState(degradedThreshold = 10)
        val warmupExecutor = Executors.newFixedThreadPool(1)
        val discarded = mutableListOf<String>()

        try {
            PoolReconciler.runReconcileTick(
                config = config,
                stateStore = stateStore,
                createOne = { "new-id" },
                onDiscardSandbox = { discarded += it },
                reconcileState = state,
                warmupExecutor = warmupExecutor,
            )
        } finally {
            warmupExecutor.shutdownNow()
        }

        assertEquals(listOf("id-1"), discarded)
        assertEquals(listOf("id-2", "id-3"), stateStore.idleIds)
    }

    private fun buildConfig(
        ownerId: String,
        maxIdle: Int,
        stateStore: PoolStateStore,
        poolName: String,
    ): PoolConfig {
        return PoolConfig.builder()
            .poolName(poolName)
            .ownerId(ownerId)
            .maxIdle(maxIdle)
            .warmupConcurrency(1)
            .stateStore(stateStore)
            .connectionConfig(ConnectionConfig.builder().build())
            .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
            .build()
    }

    private class RenewFailAfterSecondPutStore : PoolStateStore {
        private val renewCalls = AtomicInteger(0)
        val putIdleIds = mutableListOf<String>()

        override fun tryTakeIdle(poolName: String): String? = null

        override fun putIdle(
            poolName: String,
            sandboxId: String,
        ) {
            putIdleIds += sandboxId
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
        ): Boolean {
            val call = renewCalls.incrementAndGet()
            return call < 3
        }

        override fun releasePrimaryLock(
            poolName: String,
            ownerId: String,
        ) {
        }

        override fun reapExpiredIdle(
            poolName: String,
            now: Instant,
        ) {
        }

        override fun snapshotCounters(poolName: String): StoreCounters = StoreCounters(idleCount = 0)

        override fun snapshotIdleEntries(poolName: String) = emptyList<com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry>()

        override fun getMaxIdle(poolName: String): Int? = null

        override fun setMaxIdle(
            poolName: String,
            maxIdle: Int,
        ) {
        }
    }

    private class PutIdleFailStore : PoolStateStore {
        override fun tryTakeIdle(poolName: String): String? = null

        override fun putIdle(
            poolName: String,
            sandboxId: String,
        ) {
            throw RuntimeException("put failed")
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
        }

        override fun reapExpiredIdle(
            poolName: String,
            now: Instant,
        ) {
        }

        override fun snapshotCounters(poolName: String): StoreCounters = StoreCounters(idleCount = 0)

        override fun snapshotIdleEntries(poolName: String) = emptyList<com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry>()

        override fun getMaxIdle(poolName: String): Int? = null

        override fun setMaxIdle(
            poolName: String,
            maxIdle: Int,
        ) {
        }
    }

    private class AlwaysSecondaryStore : PoolStateStore {
        val putIdleIds = mutableListOf<String>()

        override fun tryTakeIdle(poolName: String): String? = null

        override fun putIdle(
            poolName: String,
            sandboxId: String,
        ) {
            putIdleIds += sandboxId
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
        ): Boolean = false

        override fun renewPrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean = false

        override fun releasePrimaryLock(
            poolName: String,
            ownerId: String,
        ) {
        }

        override fun reapExpiredIdle(
            poolName: String,
            now: Instant,
        ) {
        }

        override fun snapshotCounters(poolName: String): StoreCounters = StoreCounters(idleCount = 0)

        override fun snapshotIdleEntries(poolName: String) = emptyList<com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry>()

        override fun getMaxIdle(poolName: String): Int? = null

        override fun setMaxIdle(
            poolName: String,
            maxIdle: Int,
        ) {
        }
    }

    private class OwnerLockingStore : PoolStateStore {
        @Volatile
        private var lockOwner: String? = null
        val putIdleIds = mutableListOf<String>()

        override fun tryTakeIdle(poolName: String): String? = null

        override fun putIdle(
            poolName: String,
            sandboxId: String,
        ) {
            putIdleIds += sandboxId
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
        ): Boolean {
            val currentOwner = lockOwner
            return if (currentOwner == null || currentOwner == ownerId) {
                lockOwner = ownerId
                true
            } else {
                false
            }
        }

        override fun renewPrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean = lockOwner == ownerId

        override fun releasePrimaryLock(
            poolName: String,
            ownerId: String,
        ) {
            if (lockOwner == ownerId) {
                lockOwner = null
            }
        }

        override fun reapExpiredIdle(
            poolName: String,
            now: Instant,
        ) {
        }

        override fun snapshotCounters(poolName: String): StoreCounters = StoreCounters(idleCount = putIdleIds.size)

        override fun snapshotIdleEntries(poolName: String) = emptyList<com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry>()

        override fun getMaxIdle(poolName: String): Int? = null

        override fun setMaxIdle(
            poolName: String,
            maxIdle: Int,
        ) {
        }
    }

    private class RenewFailsOnFirstCallStore : PoolStateStore {
        private val renewCalls = AtomicInteger(0)
        val putIdleIds = mutableListOf<String>()

        override fun tryTakeIdle(poolName: String): String? = null

        override fun putIdle(
            poolName: String,
            sandboxId: String,
        ) {
            putIdleIds += sandboxId
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
        ): Boolean {
            val call = renewCalls.incrementAndGet()
            return call > 1
        }

        override fun releasePrimaryLock(
            poolName: String,
            ownerId: String,
        ) {
        }

        override fun reapExpiredIdle(
            poolName: String,
            now: Instant,
        ) {
        }

        override fun snapshotCounters(poolName: String): StoreCounters = StoreCounters(idleCount = 0)

        override fun snapshotIdleEntries(poolName: String) = emptyList<com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry>()

        override fun getMaxIdle(poolName: String): Int? = null

        override fun setMaxIdle(
            poolName: String,
            maxIdle: Int,
        ) {
        }
    }

    private open class ShrinkStore(
        idleIds: List<String>,
        private val renewFailuresAfter: Int? = null,
    ) : PoolStateStore {
        val idleIds = idleIds.toMutableList()
        private val renewCalls = AtomicInteger(0)

        override fun tryTakeIdle(poolName: String): String? {
            return if (idleIds.isEmpty()) null else idleIds.removeAt(0)
        }

        override fun putIdle(
            poolName: String,
            sandboxId: String,
        ) {
        }

        override fun removeIdle(
            poolName: String,
            sandboxId: String,
        ) {
            idleIds.remove(sandboxId)
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
        ): Boolean {
            val call = renewCalls.incrementAndGet()
            return renewFailuresAfter == null || call <= renewFailuresAfter
        }

        override fun releasePrimaryLock(
            poolName: String,
            ownerId: String,
        ) {
        }

        override fun reapExpiredIdle(
            poolName: String,
            now: Instant,
        ) {
        }

        override fun snapshotCounters(poolName: String): StoreCounters = StoreCounters(idleCount = idleIds.size)

        override fun snapshotIdleEntries(poolName: String) = emptyList<com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry>()

        override fun getMaxIdle(poolName: String): Int? = null

        override fun setMaxIdle(
            poolName: String,
            maxIdle: Int,
        ) {
        }
    }

    private class SecondaryShrinkStore(
        idleIds: List<String>,
    ) : ShrinkStore(idleIds) {
        override fun tryAcquirePrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean = false
    }
}
