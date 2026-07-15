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

package com.alibaba.opensandbox.sandbox.domain.pool

import java.time.Duration
import java.time.Instant

/**
 * Abstraction for storing pool coordination state and idle sandbox membership.
 *
 * All operations are namespaced by [poolName]. Implementations must ensure:
 * - Atomic take: one idle sandbox can only be taken by one acquire.
 * - Idempotent put/remove for idle membership.
 * - tryTakeIdle should prefer FIFO (oldest idle first) as best-effort.
 *
 * In distributed mode, only the current primary lock holder may execute
 * reconcile maintenance writes (putIdle, reapExpiredIdle). Foreground
 * acquire-path writes (tryTakeIdle, removeIdle) are allowed on all nodes.
 */
interface PoolStateStore {
    /**
     * Atomically removes and returns one idle sandbox ID for the pool, or null if none.
     * Best-effort FIFO (oldest first).
     */
    fun tryTakeIdle(poolName: String): String?

    /**
     * Variant of [tryTakeIdle] that skips entries whose remaining TTL is below [minRemainingTtl].
     *
     * Atomically removes and returns a [TakeIdleResult] holding the chosen idle sandbox ID (or
     * null if none satisfied the threshold) and the IDs of any **alive** entries that were
     * skipped because their remaining TTL fell below [minRemainingTtl]. Already-expired entries
     * are silently dropped — the server has already reaped them.
     *
     * Callers should best-effort terminate every ID in [TakeIdleResult.discardedAliveSandboxIds]
     * so those sandboxes do not linger past their pool membership and consume quota until
     * server-side TTL.
     *
     * Default implementation delegates to [tryTakeIdle] for source compatibility with stores
     * that predate this method. Such stores cannot surface near-expiry entries, so the
     * discarded-alive list is always empty regardless of [minRemainingTtl]. Stores that
     * genuinely want near-expiry filtering must override this method.
     */
    fun tryTakeIdle(
        poolName: String,
        minRemainingTtl: Duration,
    ): TakeIdleResult = TakeIdleResult.of(tryTakeIdle(poolName))

    /**
     * Adds a sandbox ID to the idle set for the pool.
     * Idempotent: duplicate put for same sandboxId leaves membership single-copy.
     */
    fun putIdle(
        poolName: String,
        sandboxId: String,
    )

    /**
     * Removes a sandbox ID from the idle set.
     * Idempotent: duplicate remove is no-op.
     */
    fun removeIdle(
        poolName: String,
        sandboxId: String,
    )

    /**
     * Tries to acquire the primary (leader) lock for this pool.
     * Best-effort mutually exclusive by poolName. Returns true if this node is now primary.
     */
    fun tryAcquirePrimaryLock(
        poolName: String,
        ownerId: String,
        ttl: Duration,
    ): Boolean

    /**
     * Renews the primary lock for the current owner. Non-owner renew is rejected.
     */
    fun renewPrimaryLock(
        poolName: String,
        ownerId: String,
        ttl: Duration,
    ): Boolean

    /**
     * Releases the primary lock for the given owner.
     */
    fun releasePrimaryLock(
        poolName: String,
        ownerId: String,
    )

    /**
     * Removes expired idle entries. In-memory store performs sweep; TTL-backed stores may no-op.
     */
    fun reapExpiredIdle(
        poolName: String,
        now: Instant,
    )

    /**
     * Variant of [reapExpiredIdle] that also evicts entries whose remaining TTL is below
     * [minRemainingTtl]. Reconcile calls this so near-expiry entries are reclaimed proactively
     * (rather than waiting for them to fully expire), letting the pool replenish them with fresh
     * sandboxes before a future acquire would discard them.
     *
     * Returns the IDs of **alive** entries that were evicted because their remaining TTL fell
     * below [minRemainingTtl]. Already-expired entries are silently dropped — the server has
     * already reaped them. Callers should best-effort terminate every returned ID so those
     * sandboxes do not linger past their pool membership.
     *
     * Default implementation delegates to [reapExpiredIdle] for source compatibility with stores
     * that predate this method. Such stores cannot surface near-expiry entries; they return an
     * empty list regardless of [minRemainingTtl]. Stores that genuinely want near-expiry
     * filtering must override this method.
     */
    fun reapExpiredIdle(
        poolName: String,
        now: Instant,
        minRemainingTtl: Duration,
    ): List<String> {
        reapExpiredIdle(poolName, now)
        return emptyList()
    }

    /**
     * Returns a snapshot of counters for the pool (at least idle count).
     * Eventually consistent for distributed stores.
     */
    fun snapshotCounters(poolName: String): StoreCounters

    /**
     * Returns a point-in-time snapshot of current idle entries for the pool.
     * Ordering should follow the store's best-effort borrow order when possible.
     */
    fun snapshotIdleEntries(poolName: String): List<IdleEntry>

    /**
     * Returns the cluster-wide max idle target for the pool, if set.
     * Used in distributed mode so that [resize] on any node is visible to the leader.
     * Return null to use the calling node's local config (e.g. single-node InMemory returns null).
     */
    fun getMaxIdle(poolName: String): Int?

    /**
     * Sets the cluster-wide max idle target for the pool.
     * In distributed mode, all nodes (including the leader) should use this for reconcile.
     * Single-node implementations may no-op; the pool falls back to local [currentMaxIdle].
     */
    fun setMaxIdle(
        poolName: String,
        maxIdle: Int,
    )

    /**
     * Configures idle-entry TTL semantics for the given pool.
     * Default is no-op so existing distributed stores can opt in explicitly.
     */
    fun setIdleEntryTtl(
        poolName: String,
        idleTtl: Duration,
    ) {
        // Default no-op.
    }

    /**
     * Returns shared destroy state for this pool namespace.
     *
     * Stores that do not support destroy fencing should return ACTIVE. Destroy management APIs
     * may still reject such stores when they cannot provide the stronger lifecycle semantics.
     */
    fun getDestroyState(poolName: String): PoolDestroyState = PoolDestroyState.ACTIVE

    /**
     * Starts a FORCE destroy operation by writing a DESTROYING fence.
     */
    fun beginDestroy(
        poolName: String,
        ownerId: String,
    ) {
        throw UnsupportedOperationException("Pool destroy is not supported by this PoolStateStore")
    }

    /**
     * Clears persistent coordination data for the pool while preserving destroy state.
     */
    fun clearPoolState(poolName: String) {
        throw UnsupportedOperationException("Pool destroy is not supported by this PoolStateStore")
    }

    /**
     * Finishes destroy by writing the DESTROYED tombstone.
     */
    fun markDestroyed(
        poolName: String,
        ownerId: String,
        tombstoneTtl: Duration?,
    ) {
        throw UnsupportedOperationException("Pool destroy is not supported by this PoolStateStore")
    }
}
