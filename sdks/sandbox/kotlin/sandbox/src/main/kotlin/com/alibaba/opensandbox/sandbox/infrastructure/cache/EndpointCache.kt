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
import java.time.Duration
import java.util.LinkedHashMap
import java.util.concurrent.locks.ReentrantLock
import kotlin.concurrent.withLock

data class EndpointCacheKey(
    val sandboxId: String,
    val port: Int,
    val useServerProxy: Boolean,
)

private data class CacheEntry(
    val endpoint: SandboxEndpoint,
    val expiresAtNanos: Long,
)

/**
 * Thread-safe LRU + TTL endpoint cache with inflight deduplication.
 */
class EndpointCache(
    maxSize: Int = 1024,
    private val ttl: Duration = Duration.ofSeconds(600),
) {
    private val maxSize = maxOf(1, maxSize)
    private val lock = ReentrantLock()
    private var generation = 0L

    private val cache =
        object : LinkedHashMap<EndpointCacheKey, CacheEntry>(16, 0.75f, true) {
            override fun removeEldestEntry(eldest: MutableMap.MutableEntry<EndpointCacheKey, CacheEntry>?): Boolean {
                return size > this@EndpointCache.maxSize
            }
        }

    private val inflight = mutableMapOf<EndpointCacheKey, InflightEntry>()

    private fun cloneEndpoint(ep: SandboxEndpoint) = SandboxEndpoint(endpoint = ep.endpoint, headers = ep.headers.toMap())

    private class InflightEntry {
        @Volatile var result: SandboxEndpoint? = null

        @Volatile var error: Exception? = null
        val latch = java.util.concurrent.CountDownLatch(1)
    }

    fun get(key: EndpointCacheKey): SandboxEndpoint? =
        lock.withLock {
            val entry = cache[key] ?: return null
            if (System.nanoTime() >= entry.expiresAtNanos) {
                cache.remove(key)
                return null
            }
            cloneEndpoint(entry.endpoint)
        }

    fun put(
        key: EndpointCacheKey,
        endpoint: SandboxEndpoint,
    ) = lock.withLock {
        cache[key] = CacheEntry(cloneEndpoint(endpoint), System.nanoTime() + ttl.toNanos())
    }

    fun invalidate(sandboxId: String) =
        lock.withLock {
            generation++
            cache.keys.removeAll { it.sandboxId == sandboxId }
            inflight.keys.removeAll { it.sandboxId == sandboxId }
        }

    fun getOrFetch(
        key: EndpointCacheKey,
        fetch: () -> SandboxEndpoint,
    ): SandboxEndpoint {
        get(key)?.let { return it }

        val existingInflight: InflightEntry?
        val myInflight: InflightEntry?
        val genBefore: Long

        lock.withLock {
            // Double-check
            val entry = cache[key]
            if (entry != null && System.nanoTime() < entry.expiresAtNanos) {
                return cloneEndpoint(entry.endpoint)
            }
            if (entry != null) cache.remove(key)

            val existing = inflight[key]
            if (existing != null) {
                existingInflight = existing
                myInflight = null
                genBefore = -1
            } else {
                val inf = InflightEntry()
                inflight[key] = inf
                existingInflight = null
                myInflight = inf
                genBefore = generation
            }
        }

        if (existingInflight != null) {
            existingInflight.latch.await()
            existingInflight.error?.let { throw it }
            return existingInflight.result!!
        }

        try {
            val endpoint = fetch()
            lock.withLock {
                if (generation == genBefore) {
                    cache[key] = CacheEntry(cloneEndpoint(endpoint), System.nanoTime() + ttl.toNanos())
                }
            }
            myInflight!!.result = endpoint
            return endpoint
        } catch (e: Exception) {
            myInflight!!.error = e
            throw e
        } finally {
            lock.withLock { inflight.remove(key) }
            myInflight!!.latch.countDown()
        }
    }

    val size: Int get() = lock.withLock { cache.size }
}
