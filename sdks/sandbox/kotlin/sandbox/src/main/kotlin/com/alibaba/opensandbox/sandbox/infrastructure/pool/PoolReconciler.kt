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
import java.time.Instant
import java.util.concurrent.Callable
import java.util.concurrent.ExecutorService

/**
 * Runs one reconcile tick: leader-gated replenish/shrink and TTL reap.
 *
 * Only the current primary lock holder performs idle maintenance writes.
 * Leader does not voluntarily release the lock; it is only lost when renew fails or TTL expires.
 * Call from a periodic scheduler; [createOne] should call lifecycle create and return the new sandbox ID or null on failure.
 */
internal object PoolReconciler {
    private val logger = LoggerFactory.getLogger(PoolReconciler::class.java)

    /**
     * Runs a single reconcile tick. If this node does not hold the primary lock, returns immediately.
     * Otherwise: reaps expired idle, snapshots counters, then shrinks excess idle or creates up to
     * min(deficit, warmupConcurrency) sandboxes via [createOne] concurrently using [warmupExecutor].
     * Lock is not released at end of tick (distributed implementations rely on TTL or renew failure to release).
     */
    fun runReconcileTick(
        config: PoolConfig,
        stateStore: PoolStateStore,
        createOne: () -> String?,
        onDiscardSandbox: (String) -> Unit = {},
        reconcileState: ReconcileState,
        warmupExecutor: ExecutorService,
    ) {
        val poolName = config.poolName
        val ownerId = config.ownerId
        val ttl = config.primaryLockTtl

        if (!stateStore.tryAcquirePrimaryLock(poolName, ownerId, ttl)) {
            logger.trace("Reconcile skip (not primary): pool_name={}", poolName)
            return
        }
        runPrimaryReplenishOnce(config, stateStore, createOne, onDiscardSandbox, reconcileState, warmupExecutor)
        // Do not release primary lock here; leader holds until renew fails or TTL expires.
    }

    private fun runPrimaryReplenishOnce(
        config: PoolConfig,
        stateStore: PoolStateStore,
        createOne: () -> String?,
        onDiscardSandbox: (String) -> Unit,
        reconcileState: ReconcileState,
        warmupExecutor: ExecutorService,
    ) {
        val poolName = config.poolName
        val ownerId = config.ownerId
        val ttl = config.primaryLockTtl
        val now = Instant.now()

        stateStore.reapExpiredIdle(poolName, now)
        val counters = stateStore.snapshotCounters(poolName)
        val excess = (counters.idleCount - config.maxIdle).coerceAtLeast(0)
        val toRemove = minOf(excess, config.warmupConcurrency)
        if (toRemove > 0) {
            shrinkExcessIdle(config, stateStore, onDiscardSandbox, toRemove)
            return
        }

        val deficit = (config.maxIdle - counters.idleCount).coerceAtLeast(0)
        val toCreate = minOf(deficit, config.warmupConcurrency)

        if (toCreate == 0 || reconcileState.isBackoffActive(now)) {
            stateStore.renewPrimaryLock(poolName, ownerId, ttl)
            logger.debug(
                "Reconcile tick: pool_name={} idle={} deficit={} toCreate=0 (backoff={})",
                poolName,
                counters.idleCount,
                deficit,
                reconcileState.isBackoffActive(now),
            )
            return
        }

        logger.debug(
            "Reconcile tick: pool_name={} idle={} deficit={} toCreate={}",
            poolName,
            counters.idleCount,
            deficit,
            toCreate,
        )

        if (!stateStore.renewPrimaryLock(poolName, ownerId, ttl)) return
        val tasks = List(toCreate) { Callable { createOne() } }
        val results: List<Pair<String?, String?>> =
            try {
                warmupExecutor.invokeAll(tasks).map { f ->
                    try {
                        f.get() to null
                    } catch (e: Exception) {
                        null to e.message
                    }
                }
            } catch (_: InterruptedException) {
                Thread.currentThread().interrupt()
                logger.info("Reconcile interrupted while waiting warmup tasks: pool_name={}", poolName)
                return
            }

        val createdSandboxIds = mutableListOf<String>()
        var failureCount = 0
        var lastError: String? = null
        for ((newId, errorMessage) in results) {
            if (newId != null) {
                createdSandboxIds += newId
            } else {
                failureCount++
                lastError = errorMessage
            }
        }
        reconcileState.recordFailures(failureCount, lastError)

        var created = 0
        for (index in createdSandboxIds.indices) {
            val sandboxId = createdSandboxIds[index]
            if (!stateStore.renewPrimaryLock(poolName, ownerId, ttl)) {
                val orphanedCount = createdSandboxIds.size - index
                for (orphanedIndex in index until createdSandboxIds.size) {
                    try {
                        onDiscardSandbox(createdSandboxIds[orphanedIndex])
                    } catch (e: Exception) {
                        logger.warn(
                            "Reconcile orphaned sandbox cleanup failed: pool_name={} sandbox_id={} error={}",
                            poolName,
                            createdSandboxIds[orphanedIndex],
                            e.message,
                        )
                    }
                }
                logger.warn(
                    "Reconcile lost primary lock before putIdle; dropped {} newly created sandbox(es): pool_name={}",
                    orphanedCount,
                    poolName,
                )
                return
            }
            try {
                stateStore.putIdle(poolName, sandboxId)
                created++
                reconcileState.recordSuccess()
            } catch (e: Exception) {
                reconcileState.recordFailure(e.message)
                val orphanedCount = createdSandboxIds.size - index
                for (orphanedIndex in index until createdSandboxIds.size) {
                    val orphanedId = createdSandboxIds[orphanedIndex]
                    try {
                        stateStore.removeIdle(poolName, orphanedId)
                    } catch (_: Exception) {
                        // best-effort remove; continue cleanup path
                    }
                    try {
                        onDiscardSandbox(orphanedId)
                    } catch (cleanupError: Exception) {
                        logger.warn(
                            "Reconcile orphaned sandbox cleanup failed after putIdle error: pool_name={} sandbox_id={} error={}",
                            poolName,
                            orphanedId,
                            cleanupError.message,
                        )
                    }
                }
                logger.warn(
                    "Reconcile putIdle failed; dropped {} newly created sandbox(es): pool_name={} error={}",
                    orphanedCount,
                    poolName,
                    e.message,
                )
                return
            }
        }

        if (created > 0) {
            logger.debug("Reconcile created {} sandboxes: pool_name={}", created, poolName)
        }
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
