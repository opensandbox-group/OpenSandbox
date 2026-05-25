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

import com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry
import com.alibaba.opensandbox.sandbox.domain.pool.PoolStateStore
import com.alibaba.opensandbox.sandbox.domain.pool.StoreCounters
import java.time.Duration
import java.time.Instant
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.ConcurrentLinkedQueue

/**
 * In-memory implementation of [PoolStateStore] for single-node use.
 *
 * Concurrency is provided entirely by concurrent data structures:
 * - Per-pool state uses [ConcurrentHashMap] (sandboxId -> [IdleEntry]) and
 *   [ConcurrentLinkedQueue] (FIFO sandboxIds). No external lock.
 * - Primary lock is a no-op: single-node mode always treats the caller as leader
 *   ([tryAcquirePrimaryLock]/[renewPrimaryLock] return true, [releasePrimaryLock] is no-op).
 *
 * Idle entries use a configurable TTL (default 24h); expired entries are removed on take, put, reap, or snapshot.
 * [tryTakeIdle] returns oldest (FIFO) non-expired idle sandbox ID.
 */
class InMemoryPoolStateStore : PoolStateStore {
    private val defaultIdleTtl: Duration = Duration.ofHours(24)
    private val idleTtlByPool = ConcurrentHashMap<String, Duration>()

    /** Per pool: (map = sandboxId -> entry for idempotent put + expiry, queue = FIFO order for take). */
    private val pools = ConcurrentHashMap<String, PoolIdleState>()

    override fun tryTakeIdle(poolName: String): String? {
        val state = pools[poolName] ?: return null
        val now = Instant.now()
        while (true) {
            val sandboxId = state.queue.poll() ?: return null
            val entry = state.map.remove(sandboxId) ?: continue // already removed (e.g. by removeIdle)
            if (entry.expiresAt.isAfter(now)) return sandboxId
            // expired, discard and poll next
        }
    }

    override fun putIdle(
        poolName: String,
        sandboxId: String,
    ) {
        val state = pools.computeIfAbsent(poolName) { PoolIdleState() }
        val expiresAt = Instant.now().plus(resolveIdleTtl(poolName))
        val entry = IdleEntry(sandboxId, expiresAt)
        if (state.map.putIfAbsent(sandboxId, entry) == null) {
            state.queue.add(sandboxId)
        }
    }

    override fun removeIdle(
        poolName: String,
        sandboxId: String,
    ) {
        pools[poolName]?.map?.remove(sandboxId)
        // queue may still contain sandboxId; tryTakeIdle will skip it (map.remove returns null)
    }

    override fun tryAcquirePrimaryLock(
        poolName: String,
        ownerId: String,
        ttl: Duration,
    ): Boolean {
        // Single-node: no real lock; always grant so reconcile runs.
        return true
    }

    override fun renewPrimaryLock(
        poolName: String,
        ownerId: String,
        ttl: Duration,
    ): Boolean {
        // Single-node: no real lock; always succeed.
        return true
    }

    override fun releasePrimaryLock(
        poolName: String,
        ownerId: String,
    ) {
        // Single-node: no-op.
    }

    override fun reapExpiredIdle(
        poolName: String,
        now: Instant,
    ) {
        val state = pools[poolName] ?: return
        state.map.entries.removeIf { it.value.expiresAt <= now }
        state.queue.removeIf { sandboxId -> !state.map.containsKey(sandboxId) }
    }

    override fun snapshotCounters(poolName: String): StoreCounters {
        val state = pools[poolName] ?: return StoreCounters(idleCount = 0)
        val now = Instant.now()
        state.map.entries.removeIf { it.value.expiresAt <= now }
        state.queue.removeIf { sandboxId -> !state.map.containsKey(sandboxId) }
        return StoreCounters(idleCount = state.map.size)
    }

    override fun snapshotIdleEntries(poolName: String): List<IdleEntry> {
        val state = pools[poolName] ?: return emptyList()
        val now = Instant.now()
        state.map.entries.removeIf { it.value.expiresAt <= now }
        state.queue.removeIf { sandboxId -> !state.map.containsKey(sandboxId) }
        return state.queue.mapNotNull { sandboxId -> state.map[sandboxId] }
    }

    override fun getMaxIdle(poolName: String): Int? = null

    override fun setMaxIdle(
        poolName: String,
        maxIdle: Int,
    ) {
        // Single-node: no shared state; pool uses local currentMaxIdle.
    }

    override fun setIdleEntryTtl(
        poolName: String,
        idleTtl: Duration,
    ) {
        idleTtlByPool[poolName] = validateIdleTtl(idleTtl)
    }

    private class PoolIdleState {
        val map = ConcurrentHashMap<String, IdleEntry>()
        val queue = ConcurrentLinkedQueue<String>()
    }

    private fun validateIdleTtl(idleTtl: Duration): Duration {
        require(!idleTtl.isNegative && !idleTtl.isZero) { "idleTtl must be positive" }
        return idleTtl
    }

    private fun resolveIdleTtl(poolName: String): Duration = idleTtlByPool[poolName] ?: defaultIdleTtl
}
