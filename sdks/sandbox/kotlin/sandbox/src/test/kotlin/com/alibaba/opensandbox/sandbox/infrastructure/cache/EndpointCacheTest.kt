// Copyright 2026 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package com.alibaba.opensandbox.sandbox.infrastructure.cache

import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test
import java.time.Duration
import java.util.concurrent.CountDownLatch
import java.util.concurrent.atomic.AtomicInteger
import kotlin.concurrent.thread

class EndpointCacheTest {
    private fun ep(addr: String) = SandboxEndpoint(endpoint = addr, headers = emptyMap())

    @Test
    fun `get returns null on miss`() {
        val cache = EndpointCache(maxSize = 10, ttl = Duration.ofMinutes(1))
        val key = EndpointCacheKey("sb-1", 8080, false)
        assertNull(cache.get(key))
    }

    @Test
    fun `get returns endpoint after put`() {
        val cache = EndpointCache(maxSize = 10, ttl = Duration.ofMinutes(1))
        val key = EndpointCacheKey("sb-1", 8080, false)
        cache.put(key, ep("localhost:8080"))
        assertEquals("localhost:8080", cache.get(key)?.endpoint)
    }

    @Test
    fun `entry expires after TTL`() {
        val cache = EndpointCache(maxSize = 10, ttl = Duration.ofMillis(50))
        val key = EndpointCacheKey("sb-1", 8080, false)
        cache.put(key, ep("localhost:8080"))
        assertNotNull(cache.get(key))
        Thread.sleep(60)
        assertNull(cache.get(key))
    }

    @Test
    fun `LRU eviction when at capacity`() {
        val cache = EndpointCache(maxSize = 3, ttl = Duration.ofMinutes(1))
        for (i in 0..2) {
            cache.put(EndpointCacheKey("sb-$i", 8080, false), ep("host-$i:8080"))
        }
        // Access sb-0 to make it recently used
        cache.get(EndpointCacheKey("sb-0", 8080, false))
        // Insert 4th — sb-1 should be evicted
        cache.put(EndpointCacheKey("sb-3", 8080, false), ep("host-3:8080"))
        assertNull(cache.get(EndpointCacheKey("sb-1", 8080, false)))
        assertNotNull(cache.get(EndpointCacheKey("sb-0", 8080, false)))
    }

    @Test
    fun `invalidate removes all entries for sandbox`() {
        val cache = EndpointCache(maxSize = 10, ttl = Duration.ofMinutes(1))
        cache.put(EndpointCacheKey("sb-1", 8080, false), ep("a"))
        cache.put(EndpointCacheKey("sb-1", 18080, false), ep("b"))
        cache.put(EndpointCacheKey("sb-2", 8080, false), ep("c"))
        cache.invalidate("sb-1")
        assertNull(cache.get(EndpointCacheKey("sb-1", 8080, false)))
        assertNull(cache.get(EndpointCacheKey("sb-1", 18080, false)))
        assertNotNull(cache.get(EndpointCacheKey("sb-2", 8080, false)))
    }

    @Test
    fun `getOrFetch deduplicates concurrent requests`() {
        val cache = EndpointCache(maxSize = 10, ttl = Duration.ofMinutes(1))
        val key = EndpointCacheKey("sb-1", 8080, false)
        val fetchCount = AtomicInteger(0)
        val latch = CountDownLatch(5)

        val threads =
            (1..5).map {
                thread {
                    cache.getOrFetch(key) {
                        fetchCount.incrementAndGet()
                        Thread.sleep(50)
                        ep("result")
                    }
                    latch.countDown()
                }
            }

        latch.await()
        assertEquals(1, fetchCount.get())
    }

    @Test
    fun `getOrFetch returns cached value without calling fetcher`() {
        val cache = EndpointCache(maxSize = 10, ttl = Duration.ofMinutes(1))
        val key = EndpointCacheKey("sb-1", 8080, false)
        cache.put(key, ep("cached"))
        var called = false
        val result =
            cache.getOrFetch(key) {
                called = true
                ep("fetched")
            }
        assertEquals("cached", result.endpoint)
        assertFalse(called)
    }

    @Test
    fun `getOrFetch does not cache on error`() {
        val cache = EndpointCache(maxSize = 10, ttl = Duration.ofMinutes(1))
        val key = EndpointCacheKey("sb-1", 8080, false)
        assertThrows(RuntimeException::class.java) {
            cache.getOrFetch(key) { throw RuntimeException("network error") }
        }
        assertEquals(0, cache.size)
    }
}
