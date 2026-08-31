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

import com.alibaba.opensandbox.sandbox.domain.pool.PoolConfig
import com.alibaba.opensandbox.sandbox.domain.pool.PoolStateStore
import org.slf4j.LoggerFactory

/**
 * Runs one reconcile tick: leader-gated replenish/shrink and TTL reap.
 *
 * Only the current primary lock holder performs idle maintenance writes.
 * Leader does not voluntarily release the lock; it is only lost when renew fails or TTL expires.
 * Call from the fixed periodic scheduler. Warmup submission is non-blocking; completed warmups are
 * committed independently by the owning pool.
 */
internal object PoolReconciler {
    private val logger = LoggerFactory.getLogger(PoolReconciler::class.java)

    /**
     * Runs a single reconcile tick. If this node does not hold the primary lock, returns immediately.
     * Otherwise: reaps expired idle, snapshots counters, then shrinks excess idle or requests enough
     * warmups to fill the currently available rolling-window slots.
     * Lock is not released at end of tick (distributed implementations rely on TTL or renew failure to release).
     */
    fun runReconcileTick(
        config: PoolConfig,
        stateStore: PoolStateStore,
        onDiscardSandbox: (String) -> Unit = {},
        onPrimaryAcquired: () -> Unit = {},
        warmingCount: Int,
        submitWarmups: (Int) -> Unit,
    ): Boolean {
        val poolName = config.poolName
        val ownerId = config.ownerId
        val ttl = config.primaryLockTtl

        if (!stateStore.tryAcquirePrimaryLock(poolName, ownerId, ttl)) {
            logger.trace("Reconcile skip (not primary): pool_name={}", poolName)
            return false
        }
        onPrimaryAcquired()
        runPrimaryReplenishOnce(
            config = config,
            stateStore = stateStore,
            onDiscardSandbox = onDiscardSandbox,
            warmingCount = warmingCount,
            submitWarmups = submitWarmups,
        )
        // Do not release primary lock here; leader holds until renew fails or TTL expires.
        return true
    }

    private fun runPrimaryReplenishOnce(
        config: PoolConfig,
        stateStore: PoolStateStore,
        onDiscardSandbox: (String) -> Unit,
        warmingCount: Int,
        submitWarmups: (Int) -> Unit,
    ) {
        val poolName = config.poolName
        val ownerId = config.ownerId
        val ttl = config.primaryLockTtl

        val discardedAlive = stateStore.reapExpiredIdle(poolName, java.time.Instant.now(), config.acquireMinRemainingTtl)
        for (sandboxId in discardedAlive) {
            // Reaped near-expiry but server-side TTL has not yet elapsed; kill so the live sandbox
            // does not linger past its pool membership and consume quota.
            onDiscardSandbox(sandboxId)
        }
        val counters = stateStore.snapshotCounters(poolName)
        val excess = (counters.idleCount - config.maxIdle).coerceAtLeast(0)
        val toRemove = minOf(excess, config.warmupConcurrency)
        if (toRemove > 0) {
            shrinkExcessIdle(config, stateStore, onDiscardSandbox, toRemove)
            return
        }

        val plan =
            WarmupPlan.calculate(
                idleCount = counters.idleCount,
                warmingCount = warmingCount,
                maxIdle = config.maxIdle,
                warmupCreateQps = config.warmupCreateQps,
            )

        if (plan.toSubmit == 0) {
            stateStore.renewPrimaryLock(poolName, ownerId, ttl)
            logger.debug(
                "Reconcile tick: pool_name={} idle={} warming={} deficit={} to_submit=0",
                poolName,
                counters.idleCount,
                warmingCount,
                plan.deficit,
            )
            return
        }

        logger.debug(
            "Reconcile tick: pool_name={} idle={} warming={} deficit={} create_qps={} to_submit={}",
            poolName,
            counters.idleCount,
            warmingCount,
            plan.deficit,
            config.warmupCreateQps,
            plan.toSubmit,
        )

        if (!stateStore.renewPrimaryLock(poolName, ownerId, ttl)) return
        submitWarmups(plan.toSubmit)
    }

    private fun shrinkExcessIdle(
        config: PoolConfig,
        stateStore: PoolStateStore,
        onDiscardSandbox: (String) -> Unit,
        toRemove: Int,
    ) {
        val poolName = config.poolName
        val ownerId = config.ownerId
        val ttl = config.primaryLockTtl
        var removed = 0

        repeat(toRemove) {
            if (!stateStore.renewPrimaryLock(poolName, ownerId, ttl)) {
                logger.warn(
                    "Reconcile lost primary lock before shrinking idle: pool_name={} removed={}",
                    poolName,
                    removed,
                )
                return
            }
            val sandboxId = stateStore.tryTakeIdle(poolName) ?: return
            try {
                onDiscardSandbox(sandboxId)
            } catch (e: Exception) {
                logger.warn(
                    "Reconcile shrink sandbox cleanup failed: pool_name={} sandbox_id={} error={}",
                    poolName,
                    sandboxId,
                    e.message,
                )
            }
            removed++
        }

        stateStore.renewPrimaryLock(poolName, ownerId, ttl)
        logger.debug("Reconcile shrunk {} idle sandbox(es): pool_name={}", removed, poolName)
    }
}
