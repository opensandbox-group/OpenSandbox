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
import com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry
import com.alibaba.opensandbox.sandbox.domain.pool.PoolDestroyState
import com.alibaba.opensandbox.sandbox.domain.pool.PoolStateStore
import com.alibaba.opensandbox.sandbox.domain.pool.StoreCounters
import com.alibaba.opensandbox.sandbox.domain.pool.TakeIdleResult
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
    private val destroyStateByPool = ConcurrentHashMap<String, DestroyStateEntry>()

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

    override fun tryTakeIdle(
        poolName: String,
        minRemainingTtl: Duration,
    ): TakeIdleResult {
        if (minRemainingTtl.isNegative || minRemainingTtl.isZero) {
            return TakeIdleResult.of(tryTakeIdle(poolName))
        }
        val state = pools[poolName] ?: return TakeIdleResult.EMPTY
        val now = Instant.now()
        val cutoff = now.plus(minRemainingTtl)
        var discardedAlive: MutableList<String>? = null
        while (true) {
            val sandboxId =
                state.queue.poll() ?: return TakeIdleResult(
                    sandboxId = null,
                    discardedAliveSandboxIds = discardedAlive ?: emptyList(),
                )
            val entry = state.map.remove(sandboxId) ?: continue // already removed (e.g. by removeIdle)
            if (entry.expiresAt.isAfter(cutoff)) {
                return TakeIdleResult(
                    sandboxId = sandboxId,
                    discardedAliveSandboxIds = discardedAlive ?: emptyList(),
                )
            }
            // Below threshold. If still alive (server-side TTL not yet elapsed), surface it so
            // the caller can kill it; otherwise silently drop — the server has already reaped it.
            if (entry.expiresAt.isAfter(now)) {
                (discardedAlive ?: ArrayList<String>().also { discardedAlive = it }).add(sandboxId)
            }
        }
    }

    override fun putIdle(
        poolName: String,
        sandboxId: String,
    ) {
        synchronized(this) {
            rejectIfDestroyed(poolName)
            val state = pools.computeIfAbsent(poolName) { PoolIdleState() }
            val expiresAt = Instant.now().plus(resolveIdleTtl(poolName))
            val entry = IdleEntry(sandboxId, expiresAt)
            if (state.map.putIfAbsent(sandboxId, entry) == null) {
                state.queue.add(sandboxId)
            }
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
        if (getDestroyState(poolName) != PoolDestroyState.ACTIVE) return false
        // Single-node: no real lock; always grant so reconcile runs.
        return true
    }

    override fun renewPrimaryLock(
        poolName: String,
        ownerId: String,
        ttl: Duration,
    ): Boolean {
        if (getDestroyState(poolName) != PoolDestroyState.ACTIVE) return false
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

    override fun reapExpiredIdle(
        poolName: String,
        now: Instant,
        minRemainingTtl: Duration,
    ): List<String> {
        if (minRemainingTtl.isNegative || minRemainingTtl.isZero) {
            reapExpiredIdle(poolName, now)
            return emptyList()
        }
        val state = pools[poolName] ?: return emptyList()
        val cutoff = now.plus(minRemainingTtl)
        var discardedAlive: MutableList<String>? = null
        // Snapshot to avoid mutating the map while iterating.
        for ((sandboxId, entry) in state.map.entries.toList()) {
            if (entry.expiresAt.isAfter(cutoff)) continue
            if (state.map.remove(sandboxId, entry)) {
                if (entry.expiresAt.isAfter(now)) {
                    (discardedAlive ?: ArrayList<String>().also { discardedAlive = it }).add(sandboxId)
                }
            }
        }
        state.queue.removeIf { sandboxId -> !state.map.containsKey(sandboxId) }
        return discardedAlive ?: emptyList()
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
        rejectIfDestroyed(poolName)
        // Single-node: no shared state; pool uses local currentMaxIdle.
    }

    override fun setIdleEntryTtl(
        poolName: String,
        idleTtl: Duration,
    ) {
        rejectIfDestroyed(poolName)
        idleTtlByPool[poolName] = validateIdleTtl(idleTtl)
    }

    override fun getDestroyState(poolName: String): PoolDestroyState =
        synchronized(this) {
            val entry = destroyStateByPool[poolName] ?: return PoolDestroyState.ACTIVE
            val expiresAt = entry.expiresAt
            if (expiresAt != null && !expiresAt.isAfter(Instant.now())) {
                destroyStateByPool.remove(poolName, entry)
                return PoolDestroyState.ACTIVE
            }
            entry.state
        }

    override fun beginDestroy(
        poolName: String,
        ownerId: String,
    ) {
        synchronized(this) {
            require(ownerId.isNotBlank()) { "ownerId must not be blank" }
            val existing = getDestroyState(poolName)
            if (existing == PoolDestroyState.DESTROYED) {
                throw PoolDestroyedException("Pool namespace is already DESTROYED: poolName=$poolName")
            }
            destroyStateByPool[poolName] = DestroyStateEntry(PoolDestroyState.DESTROYING, null, ownerId)
        }
    }

    override fun clearPoolState(poolName: String) {
        synchronized(this) {
            pools.remove(poolName)
            idleTtlByPool.remove(poolName)
        }
    }

    override fun markDestroyed(
        poolName: String,
        ownerId: String,
        tombstoneTtl: Duration?,
    ) {
        synchronized(this) {
            require(ownerId.isNotBlank()) { "ownerId must not be blank" }
            val expiresAt = tombstoneTtl?.let { Instant.now().plus(it) }
            destroyStateByPool[poolName] = DestroyStateEntry(PoolDestroyState.DESTROYED, expiresAt, ownerId)
        }
    }

    private class PoolIdleState {
        val map = ConcurrentHashMap<String, IdleEntry>()
        val queue = ConcurrentLinkedQueue<String>()
    }

    private data class DestroyStateEntry(
        val state: PoolDestroyState,
        val expiresAt: Instant?,
        val ownerId: String,
    )

    private fun validateIdleTtl(idleTtl: Duration): Duration {
        require(!idleTtl.isNegative && !idleTtl.isZero) { "idleTtl must be positive" }
        return idleTtl
    }

    private fun resolveIdleTtl(poolName: String): Duration = idleTtlByPool[poolName] ?: defaultIdleTtl

    private fun rejectIfDestroyed(poolName: String) {
        val state = getDestroyState(poolName)
        if (state != PoolDestroyState.ACTIVE) {
            throw PoolDestroyedException("Pool namespace is $state: poolName=$poolName")
        }
    }
}
