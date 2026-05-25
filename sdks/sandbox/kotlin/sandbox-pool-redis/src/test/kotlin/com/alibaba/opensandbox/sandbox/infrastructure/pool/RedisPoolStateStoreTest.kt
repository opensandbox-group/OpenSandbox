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
    fun `setIdleEntryTtl validates positive duration`() {
        val stateStore = requireStore()

        assertThrows(IllegalArgumentException::class.java) {
            stateStore.setIdleEntryTtl(poolName, Duration.ZERO)
        }
    }

    private fun requireStore(): RedisPoolStateStore = store ?: error("Redis store was not initialized")
}
