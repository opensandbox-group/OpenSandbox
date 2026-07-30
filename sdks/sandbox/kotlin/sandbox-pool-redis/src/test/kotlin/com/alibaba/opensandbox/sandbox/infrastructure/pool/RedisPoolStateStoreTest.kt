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

import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolDestroyedException
import com.alibaba.opensandbox.sandbox.domain.pool.PoolDestroyState
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Assumptions.assumeTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import redis.clients.jedis.JedisPooled
import java.time.Duration
import java.time.Instant
import java.util.UUID

class RedisPoolStateStoreTest {
    private var redis: JedisPooled? = null
    private var store: RedisPoolStateStore? = null
    private val poolName = "test-pool"

    @BeforeEach
    fun setUp() {
        val redisUrl = System.getenv("OPENSANDBOX_TEST_REDIS_URL")
        assumeTrue(!redisUrl.isNullOrBlank(), "Set OPENSANDBOX_TEST_REDIS_URL to run RedisPoolStateStore tests")
        redis = JedisPooled(redisUrl)
        store =
            RedisPoolStateStore(
                redis = redis!!,
                keyPrefix = "opensandbox:test:${UUID.randomUUID()}",
            )
    }

    @AfterEach
    fun tearDown() {
        redis?.close()
    }

    @Test
    fun `putIdle and tryTakeIdle round-trip with FIFO order`() {
        val stateStore = requireStore()

        stateStore.putIdle(poolName, "id-1")
        stateStore.putIdle(poolName, "id-2")
        stateStore.putIdle(poolName, "id-3")

        assertEquals("id-1", stateStore.tryTakeIdle(poolName))
        assertEquals("id-2", stateStore.tryTakeIdle(poolName))
        assertEquals("id-3", stateStore.tryTakeIdle(poolName))
        assertNull(stateStore.tryTakeIdle(poolName))
    }

    @Test
    fun `putIdle is idempotent`() {
        val stateStore = requireStore()

        stateStore.putIdle(poolName, "id-1")
        stateStore.putIdle(poolName, "id-1")

        assertEquals(1, stateStore.snapshotCounters(poolName).idleCount)
        assertEquals("id-1", stateStore.tryTakeIdle(poolName))
        assertNull(stateStore.tryTakeIdle(poolName))
    }

    @Test
    fun `removeIdle is idempotent`() {
        val stateStore = requireStore()

        stateStore.putIdle(poolName, "id-1")
        stateStore.removeIdle(poolName, "id-1")
        stateStore.removeIdle(poolName, "id-1")

        assertNull(stateStore.tryTakeIdle(poolName))
    }

    @Test
    fun `reapExpiredIdle removes expired entries`() {
        val stateStore = requireStore()

        stateStore.setIdleEntryTtl(poolName, Duration.ofMillis(50))
        stateStore.putIdle(poolName, "id-1")
        Thread.sleep(100)
        assertEquals(1, stateStore.snapshotCounters(poolName).idleCount)

        stateStore.reapExpiredIdle(poolName, Instant.now())

        assertEquals(0, stateStore.snapshotCounters(poolName).idleCount)
        assertNull(stateStore.tryTakeIdle(poolName))
    }

    @Test
    fun `primary lock allows current owner and rejects non-owner`() {
        val stateStore = requireStore()

        assertTrue(stateStore.tryAcquirePrimaryLock(poolName, "owner-1", Duration.ofSeconds(60)))
        assertTrue(stateStore.tryAcquirePrimaryLock(poolName, "owner-1", Duration.ofSeconds(60)))
        assertTrue(stateStore.renewPrimaryLock(poolName, "owner-1", Duration.ofSeconds(60)))
        assertFalse(stateStore.tryAcquirePrimaryLock(poolName, "owner-2", Duration.ofSeconds(60)))
        assertFalse(stateStore.renewPrimaryLock(poolName, "owner-2", Duration.ofSeconds(60)))
    }

    @Test
    fun `releasePrimaryLock only releases current owner`() {
        val stateStore = requireStore()

        assertTrue(stateStore.tryAcquirePrimaryLock(poolName, "owner-1", Duration.ofSeconds(60)))
        stateStore.releasePrimaryLock(poolName, "owner-2")
        assertFalse(stateStore.tryAcquirePrimaryLock(poolName, "owner-2", Duration.ofSeconds(60)))

        stateStore.releasePrimaryLock(poolName, "owner-1")
        assertTrue(stateStore.tryAcquirePrimaryLock(poolName, "owner-2", Duration.ofSeconds(60)))
    }

    @Test
    fun `maxIdle is shared through Redis`() {
        val stateStore = requireStore()

        assertNull(stateStore.getMaxIdle(poolName))
        stateStore.setMaxIdle(poolName, 7)
        assertEquals(7, stateStore.getMaxIdle(poolName))
    }

    @Test
    fun `tryTakeIdle surfaces alive entries below the threshold so callers can kill them`() {
        val stateStore = requireStore()

        // 1s TTL: still alive (server-side TTL has not elapsed) but well below the 60s threshold.
        stateStore.setIdleEntryTtl(poolName, Duration.ofSeconds(1))
        stateStore.putIdle(poolName, "id-1")
        stateStore.putIdle(poolName, "id-2")

        val result = stateStore.tryTakeIdle(poolName, Duration.ofSeconds(60))
        assertNull(result.sandboxId)
        assertEquals(setOf("id-1", "id-2"), result.discardedAliveSandboxIds.toSet())
        assertEquals(0, stateStore.snapshotCounters(poolName).idleCount)
    }

    @Test
    fun `tryTakeIdle silently drops fully-expired entries`() {
        val stateStore = requireStore()

        stateStore.setIdleEntryTtl(poolName, Duration.ofMillis(50))
        stateStore.putIdle(poolName, "expired")
        Thread.sleep(150)
        stateStore.setIdleEntryTtl(poolName, Duration.ofMinutes(10))
        stateStore.putIdle(poolName, "alive")

        val result = stateStore.tryTakeIdle(poolName, Duration.ofSeconds(60))
        // expired silently dropped (no kill needed); alive returned.
        assertEquals("alive", result.sandboxId)
        assertEquals(emptyList<String>(), result.discardedAliveSandboxIds)
    }

    @Test
    fun `reapExpiredIdle with minRemainingTtl returns alive evicted entries`() {
        val stateStore = requireStore()

        stateStore.setIdleEntryTtl(poolName, Duration.ofSeconds(1))
        stateStore.putIdle(poolName, "id-1")
        stateStore.putIdle(poolName, "id-2")

        val discardedAlive =
            stateStore.reapExpiredIdle(poolName, Instant.now(), Duration.ofSeconds(60))

        assertEquals(setOf("id-1", "id-2"), discardedAlive.toSet())
        assertEquals(0, stateStore.snapshotCounters(poolName).idleCount)
    }

    @Test
    fun `reapExpiredIdle with minRemainingTtl preserves entries above the threshold`() {
        val stateStore = requireStore()

        stateStore.setIdleEntryTtl(poolName, Duration.ofMinutes(10))
        stateStore.putIdle(poolName, "id-1")

        val discardedAlive =
            stateStore.reapExpiredIdle(poolName, Instant.now(), Duration.ofSeconds(60))

        assertEquals(emptyList<String>(), discardedAlive)
        assertEquals(1, stateStore.snapshotCounters(poolName).idleCount)
    }

    @Test
    fun `tryTakeIdle with minRemainingTtl returns entries that satisfy the threshold`() {
        val stateStore = requireStore()

        stateStore.setIdleEntryTtl(poolName, Duration.ofMinutes(10))
        stateStore.putIdle(poolName, "id-1")

        val result = stateStore.tryTakeIdle(poolName, Duration.ofSeconds(60))
        assertEquals("id-1", result.sandboxId)
        assertEquals(emptyList<String>(), result.discardedAliveSandboxIds)
    }

    @Test
    fun `tryTakeIdle with zero minRemainingTtl behaves like the base call`() {
        val stateStore = requireStore()

        stateStore.putIdle(poolName, "id-1")
        val taken = stateStore.tryTakeIdle(poolName, Duration.ZERO)
        assertEquals("id-1", taken.sandboxId)
        assertEquals(emptyList<String>(), taken.discardedAliveSandboxIds)

        val empty = stateStore.tryTakeIdle(poolName, Duration.ZERO)
        assertNull(empty.sandboxId)
        assertEquals(emptyList<String>(), empty.discardedAliveSandboxIds)
    }

    @Test
    fun `setIdleEntryTtl validates positive duration`() {
        val stateStore = requireStore()

        assertThrows(IllegalArgumentException::class.java) {
            stateStore.setIdleEntryTtl(poolName, Duration.ZERO)
        }
    }

    @Test
    fun `destroy state fences writes and clearPoolState keeps tombstone`() {
        val stateStore = requireStore()

        stateStore.putIdle(poolName, "id-1")
        stateStore.setMaxIdle(poolName, 3)

        stateStore.beginDestroy(poolName, "destroyer")

        assertEquals(PoolDestroyState.DESTROYING, stateStore.getDestroyState(poolName))
        assertThrows(PoolDestroyedException::class.java) {
            stateStore.putIdle(poolName, "id-2")
        }
        assertThrows(PoolDestroyedException::class.java) {
            stateStore.setMaxIdle(poolName, 4)
        }
        assertThrows(PoolDestroyedException::class.java) {
            stateStore.setIdleEntryTtl(poolName, Duration.ofMinutes(5))
        }
        assertFalse(stateStore.tryAcquirePrimaryLock(poolName, "owner-1", Duration.ofSeconds(60)))
        assertFalse(stateStore.renewPrimaryLock(poolName, "owner-1", Duration.ofSeconds(60)))
        assertEquals("id-1", stateStore.tryTakeIdle(poolName))

        stateStore.clearPoolState(poolName)
        stateStore.markDestroyed(poolName, "destroyer", Duration.ofMinutes(5))

        assertEquals(PoolDestroyState.DESTROYED, stateStore.getDestroyState(poolName))
        assertThrows(PoolDestroyedException::class.java) {
            stateStore.beginDestroy(poolName, "destroyer-2")
        }
        assertEquals(0, stateStore.snapshotCounters(poolName).idleCount)
        assertNull(stateStore.getMaxIdle(poolName))
    }

    private fun requireStore(): RedisPoolStateStore = store ?: error("Redis store was not initialized")
}
