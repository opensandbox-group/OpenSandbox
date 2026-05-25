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
}
