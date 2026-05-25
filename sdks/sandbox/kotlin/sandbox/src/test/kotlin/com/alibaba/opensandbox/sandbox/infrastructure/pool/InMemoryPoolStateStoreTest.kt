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

import com.alibaba.opensandbox.sandbox.domain.pool.PoolStateStore
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import java.time.Duration
import java.time.Instant

/**
 * Contract and behavior tests for [InMemoryPoolStateStore].
 */
class InMemoryPoolStateStoreTest {
    private lateinit var store: PoolStateStore
    private val poolName = "test-pool"

    @BeforeEach
    fun setUp() {
        store = InMemoryPoolStateStore()
    }

    @Test
    fun `tryTakeIdle returns null when empty`() {
        assertNull(store.tryTakeIdle(poolName))
    }

    @Test
    fun `putIdle and tryTakeIdle round-trip`() {
        store.putIdle(poolName, "id-1")
        assertEquals("id-1", store.tryTakeIdle(poolName))
        assertNull(store.tryTakeIdle(poolName))
    }

    @Test
    fun `tryTakeIdle prefers FIFO`() {
        store.putIdle(poolName, "id-1")
        store.putIdle(poolName, "id-2")
        store.putIdle(poolName, "id-3")
        assertEquals("id-1", store.tryTakeIdle(poolName))
        assertEquals("id-2", store.tryTakeIdle(poolName))
        assertEquals("id-3", store.tryTakeIdle(poolName))
        assertNull(store.tryTakeIdle(poolName))
    }

    @Test
    fun `removeIdle removes entry`() {
        store.putIdle(poolName, "id-1")
        store.removeIdle(poolName, "id-1")
        assertNull(store.tryTakeIdle(poolName))
    }

    @Test
    fun `removeIdle is idempotent`() {
        store.putIdle(poolName, "id-1")
        store.removeIdle(poolName, "id-1")
        store.removeIdle(poolName, "id-1")
        assertNull(store.tryTakeIdle(poolName))
    }

    @Test
    fun `putIdle is idempotent - single copy`() {
        store.putIdle(poolName, "id-1")
        store.putIdle(poolName, "id-1")
        assertEquals("id-1", store.tryTakeIdle(poolName))
        assertNull(store.tryTakeIdle(poolName))
    }

    @Test
    fun `snapshotCounters returns idle count`() {
        assertEquals(0, store.snapshotCounters(poolName).idleCount)
        store.putIdle(poolName, "id-1")
        store.putIdle(poolName, "id-2")
        assertEquals(2, store.snapshotCounters(poolName).idleCount)
        store.tryTakeIdle(poolName)
        assertEquals(1, store.snapshotCounters(poolName).idleCount)
    }

    @Test
    fun `tryAcquirePrimaryLock acquires when free`() {
        val ok = store.tryAcquirePrimaryLock(poolName, "owner-1", Duration.ofSeconds(60))
        assertTrue(ok)
    }

    @Test
    fun `tryAcquirePrimaryLock always grants in single-node mode`() {
        // InMemory is single-node: no real lock; every caller is treated as leader.
        store.tryAcquirePrimaryLock(poolName, "owner-1", Duration.ofSeconds(60))
        val ok = store.tryAcquirePrimaryLock(poolName, "owner-2", Duration.ofSeconds(60))
        assertTrue(ok)
    }

    @Test
    fun `renewPrimaryLock succeeds for owner`() {
        store.tryAcquirePrimaryLock(poolName, "owner-1", Duration.ofSeconds(60))
        assertTrue(store.renewPrimaryLock(poolName, "owner-1", Duration.ofSeconds(60)))
    }

    @Test
    fun `renewPrimaryLock always succeeds in single-node mode`() {
        // InMemory is single-node: no real lock; renew always succeeds.
        store.tryAcquirePrimaryLock(poolName, "owner-1", Duration.ofSeconds(60))
        assertTrue(store.renewPrimaryLock(poolName, "owner-2", Duration.ofSeconds(60)))
    }

    @Test
    fun `releasePrimaryLock allows new owner`() {
        store.tryAcquirePrimaryLock(poolName, "owner-1", Duration.ofSeconds(60))
        store.releasePrimaryLock(poolName, "owner-1")
        assertTrue(store.tryAcquirePrimaryLock(poolName, "owner-2", Duration.ofSeconds(60)))
    }

    @Test
    fun `pool isolation - different pools do not share idle`() {
        store.putIdle("pool-a", "id-a")
        store.putIdle("pool-b", "id-b")
        assertEquals("id-a", store.tryTakeIdle("pool-a"))
        assertEquals("id-b", store.tryTakeIdle("pool-b"))
        assertNull(store.tryTakeIdle("pool-a"))
    }

    @Test
    fun `pool isolation - different pools do not share lock`() {
        store.tryAcquirePrimaryLock("pool-a", "owner-a", Duration.ofSeconds(60))
        assertTrue(store.tryAcquirePrimaryLock("pool-b", "owner-b", Duration.ofSeconds(60)))
    }

    @Test
    fun `reapExpiredIdle removes expired entries`() {
        store.putIdle(poolName, "id-1")
        store.reapExpiredIdle(poolName, Instant.now().plus(Duration.ofHours(25)))
        assertEquals(0, store.snapshotCounters(poolName).idleCount)
    }

    @Test
    fun `custom idle ttl expires entries accordingly`() {
        val inMemoryStore = InMemoryPoolStateStore()
        inMemoryStore.setIdleEntryTtl(poolName, Duration.ofSeconds(10))
        inMemoryStore.putIdle(poolName, "id-1")
        inMemoryStore.reapExpiredIdle(poolName, Instant.now().plus(Duration.ofSeconds(11)))
        assertEquals(0, inMemoryStore.snapshotCounters(poolName).idleCount)
    }

    @Test
    fun `setIdleEntryTtl validates positive duration`() {
        val inMemoryStore = InMemoryPoolStateStore()
        assertThrows(IllegalArgumentException::class.java) {
            inMemoryStore.setIdleEntryTtl(poolName, Duration.ZERO)
        }
    }

    @Test
    fun `getMaxIdle returns null and setMaxIdle is no-op in single-node`() {
        assertNull(store.getMaxIdle(poolName))
        store.setMaxIdle(poolName, 10)
        assertNull(store.getMaxIdle(poolName))
    }

    @Test
    fun `snapshotCounters compacts queue tombstones`() {
        store.putIdle(poolName, "id-1")
        store.putIdle(poolName, "id-2")
        store.removeIdle(poolName, "id-1")
        store.snapshotCounters(poolName)

        val queueSize = extractQueueSize(store as InMemoryPoolStateStore, poolName)
        assertEquals(1, queueSize)
    }

    private fun extractQueueSize(
        inMemoryStore: InMemoryPoolStateStore,
        pool: String,
    ): Int {
        val poolsField = InMemoryPoolStateStore::class.java.getDeclaredField("pools")
        poolsField.isAccessible = true
        val pools = poolsField.get(inMemoryStore) as java.util.concurrent.ConcurrentHashMap<*, *>
        val state = pools[pool] ?: return 0
        val queueField = state.javaClass.getDeclaredField("queue")
        queueField.isAccessible = true
        val queue = queueField.get(state) as java.util.concurrent.ConcurrentLinkedQueue<*>
        return queue.size
    }
}
