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

package com.alibaba.opensandbox.sandbox.pool

import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.SandboxManager
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolAcquireFailedException
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolEmptyException
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolNotRunningException
import com.alibaba.opensandbox.sandbox.domain.pool.AcquirePolicy
import com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry
import com.alibaba.opensandbox.sandbox.domain.pool.PoolConfig
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec
import com.alibaba.opensandbox.sandbox.domain.pool.PoolLifecycleState
import com.alibaba.opensandbox.sandbox.domain.pool.PoolSnapshot
import com.alibaba.opensandbox.sandbox.domain.pool.PoolState
import com.alibaba.opensandbox.sandbox.domain.pool.PoolStateStore
import com.alibaba.opensandbox.sandbox.domain.pool.SandboxPreparer
import com.alibaba.opensandbox.sandbox.infrastructure.pool.PoolReconciler
import com.alibaba.opensandbox.sandbox.infrastructure.pool.ReconcileState
import org.slf4j.LoggerFactory
import java.time.Duration
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicReference
import java.util.concurrent.locks.Condition
import java.util.concurrent.locks.ReentrantLock

/**
 * Client-side sandbox pool for acquiring ready sandboxes with predictable latency.
 *
 * The pool maintains an idle buffer of clean, borrowable sandboxes. Callers [acquire] a sandbox,
 * use it, and terminate it via [Sandbox.kill] when done. No return/finalize API; sandboxes are ephemeral.
 *
 * Uses [PoolStateStore] for idle membership and primary lock; runs a background reconcile loop
 * when started. Replenish is leader-gated; acquire is allowed on all nodes.
 *
 * ## Usage
 *
 * ```kotlin
 * val pool = SandboxPool.builder()
 *     .poolName("my-pool")
 *     .ownerId("worker-1")
 *     .maxIdle(5)
 *     .stateStore(InMemoryPoolStateStore())
 *     .connectionConfig(connectionConfig)
 *     .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
 *     .build()
 * pool.start()
 *
 * val sandbox = pool.acquire(sandboxTimeout = Duration.ofMinutes(30), policy = AcquirePolicy.DIRECT_CREATE)
 * try {
 *     // use sandbox
 * } finally {
 *     sandbox.kill()
 * }
 *
 * pool.shutdown(graceful = true)
 * ```
 *
 * @see PoolConfig
 */
class SandboxPool internal constructor(
    config: PoolConfig,
    private val sandboxManagerFactory: (ConnectionConfig) -> SandboxManager = { cfg ->
        SandboxManager.builder().connectionConfig(cfg).build()
    },
) {
    private val logger = LoggerFactory.getLogger(SandboxPool::class.java)

    private val config: PoolConfig = config
    private val stateStore: PoolStateStore = config.stateStore
    private val connectionConfig: ConnectionConfig = config.connectionConfig
    private val creationSpec: PoolCreationSpec = config.creationSpec
    private val reconcileState = ReconcileState(config.degradedThreshold)

    @Volatile
    private var currentMaxIdle: Int = config.maxIdle

    private val lifecycleState = AtomicReference(LifecycleState.NOT_STARTED)
    private var sandboxManager: SandboxManager? = null
    private var scheduler: ScheduledExecutorService? = null
    private var warmupExecutor: ExecutorService? = null
    private var reconcileTask: ScheduledFuture<*>? = null
    private val inFlightOperations = AtomicInteger(0)
    private val inFlightLock = ReentrantLock()
    private val inFlightZero: Condition = inFlightLock.newCondition()

    /**
     * Starts the pool: begins the background reconcile loop and, if [PoolConfig.maxIdle] > 0,
     * triggers an immediate warmup tick.
     */
    @Synchronized
    fun start() {
        if (lifecycleState.get() == LifecycleState.RUNNING || lifecycleState.get() == LifecycleState.STARTING) {
            return
        }
        lifecycleState.set(LifecycleState.STARTING)
        try {
            warnIfPrimaryLockTtlMayExpireDuringWarmup()
            sandboxManager = createSandboxManager()
            stateStore.setIdleEntryTtl(config.poolName, config.idleTimeout)
            stateStore.setMaxIdle(config.poolName, config.maxIdle)
            warmupExecutor =
                Executors.newFixedThreadPool(config.warmupConcurrency.coerceAtLeast(1)) { r ->
                    Thread(r, "sandbox-pool-warmup-${config.poolName}").apply { isDaemon = true }
                }
            val exec =
                Executors.newSingleThreadScheduledExecutor { r ->
                    Thread(r, "sandbox-pool-reconcile-${config.poolName}").apply { isDaemon = true }
                }
            scheduler = exec
            val reconcileIntervalMs = config.reconcileInterval.toMillis()
            reconcileTask =
                exec.scheduleAtFixedRate(
                    {
                        try {
                            runReconcileTick()
                        } catch (t: Throwable) {
                            // Keep periodic scheduling alive even if one tick fails unexpectedly.
                            logger.error("Pool reconcile tick failed unexpectedly: pool_name={}", config.poolName, t)
                        }
                    },
                    if (config.maxIdle > 0) 0 else reconcileIntervalMs,
                    reconcileIntervalMs,
                    TimeUnit.MILLISECONDS,
                )
            lifecycleState.set(LifecycleState.RUNNING)
            logger.info(
                "Pool started: pool_name={} state={} maxIdle={}",
                config.poolName,
                LifecycleState.RUNNING,
                currentMaxIdle,
            )
        } catch (e: Exception) {
            stopReconcile()
            closeProvider()
            lifecycleState.set(LifecycleState.STOPPED)
            logger.error("Pool start failed: pool_name={}", config.poolName, e)
            throw e
        }
    }

    private fun warnIfPrimaryLockTtlMayExpireDuringWarmup() {
        if (config.primaryLockTtl > config.warmupReadyTimeout) return
        logger.warn(
            "Pool primary lock TTL may expire during warmup: pool_name={} primary_lock_ttl_ms={} " +
                "warmup_ready_timeout_ms={}. In distributed mode, configure primaryLockTtl greater than " +
                "warmupReadyTimeout plus expected warmupSandboxPreparer time and buffer to avoid losing leadership " +
                "while creating idle sandboxes.",
            config.poolName,
            config.primaryLockTtl.toMillis(),
            config.warmupReadyTimeout.toMillis(),
        )
    }

    /**
     * Acquires a sandbox from the pool or creates one directly per policy.
     *
     * 1. Tries to take an idle sandbox ID from the store and connect.
     * 2. If connect fails (stale ID), removes the ID, best-effort kill, then falls back to direct create.
     * 3. Under [AcquirePolicy.FAIL_FAST]:
     *    - throws [PoolEmptyException] when idle buffer is empty;
     *    - throws [PoolAcquireFailedException] when an idle candidate exists but connect fails.
     * 4. If no idle and [policy] is [AcquirePolicy.DIRECT_CREATE], creates a new sandbox via lifecycle API and returns it.
     * 5. If pool is not RUNNING (e.g. DRAINING/STOPPED), throws [PoolNotRunningException].
     *
     * @param sandboxTimeout Optional duration to set on the acquired sandbox (applied via renew after connect).
     * @param policy Behavior when idle buffer is empty (default: [AcquirePolicy.DIRECT_CREATE]).
     * @return A connected [Sandbox] instance. Caller must call [Sandbox.kill] when done.
     * @throws PoolNotRunningException when pool lifecycle state is not RUNNING.
     * @throws PoolEmptyException when policy is FAIL_FAST and idle is empty.
     * @throws PoolAcquireFailedException when policy is FAIL_FAST and idle candidate is unusable.
     * @throws SandboxException for lifecycle create/connect/renew errors.
     */
    fun acquire(
        sandboxTimeout: Duration? = null,
        policy: AcquirePolicy = AcquirePolicy.DIRECT_CREATE,
    ): Sandbox {
        if (lifecycleState.get() != LifecycleState.RUNNING) {
            val state = lifecycleState.get()
            logger.info("Pool not running, acquire rejected: pool_name={} state={}", config.poolName, state)
            throw PoolNotRunningException("Cannot acquire when pool state is $state")
        }
        beginOperation()
        try {
            if (lifecycleState.get() != LifecycleState.RUNNING) {
                val state = lifecycleState.get()
                logger.info("Pool not running after acquire started, rejected: pool_name={} state={}", config.poolName, state)
                throw PoolNotRunningException("Cannot acquire when pool state is $state")
            }
            val poolName = config.poolName
            val sandboxId = stateStore.tryTakeIdle(poolName)
            var noIdleReason: String? = null // null = got a sandbox from idle; non-null = reason we have no usable idle
            var idleConnectFailure: Exception? = null
            if (sandboxId != null) {
                try {
                    val sandbox =
                        Sandbox.connector()
                            .sandboxId(sandboxId)
                            .connectTimeout(config.acquireReadyTimeout)
                            .healthCheckPollingInterval(config.acquireHealthCheckPollingInterval)
                            .skipHealthCheck(config.acquireSkipHealthCheck)
                            .connectionConfig(connectionConfig)
                            .run {
                                config.acquireHealthCheck?.let { healthCheck(it) } ?: this
                            }.connect()
                    sandboxTimeout?.let { sandbox.renew(it) }
                    logger.debug(
                        "Acquire from idle: pool_name={} sandbox_id={} policy={}",
                        poolName,
                        sandboxId,
                        policy,
                    )
                    return sandbox
                } catch (e: Exception) {
                    idleConnectFailure = e
                    logger.warn(
                        "Idle connect failed (stale or unreachable), removed from pool and falling back: " +
                            "pool_name={} sandbox_id={} error={}",
                        poolName,
                        sandboxId,
                        e.message,
                    )
                    stateStore.removeIdle(poolName, sandboxId)
                    try {
                        sandboxManager?.killSandbox(sandboxId)
                    } catch (_: Exception) {
                        // best-effort kill; do not replace original error
                    }
                    noIdleReason = "idle connect failed for sandbox_id=$sandboxId (stale or unreachable)"
                }
            } else {
                noIdleReason = "idle buffer empty"
            }
            val reason = noIdleReason!!
            if (policy == AcquirePolicy.FAIL_FAST) {
                logger.debug("Acquire FAIL_FAST: pool_name={} reason={}", poolName, reason)
                if (sandboxId != null) {
                    throw PoolAcquireFailedException(
                        message = "Cannot acquire: $reason; policy is FAIL_FAST",
                        cause = idleConnectFailure,
                    )
                }
                throw PoolEmptyException("Cannot acquire: $reason; policy is FAIL_FAST")
            }
            logger.debug("Acquire direct create: pool_name={} reason={} policy={}", poolName, reason, policy)
            return directCreate(sandboxTimeout)
        } finally {
            endOperation()
        }
    }

    /**
     * Updates the maximum idle target. In distributed mode the new value is written to the store
     * so the whole cluster (including the leader) uses it; in single-node only this process sees it.
     * This method can be called from any node. Actual replenish or shrink work is performed
     * asynchronously by the current primary during periodic reconcile.
     */
    fun resize(maxIdle: Int) {
        require(maxIdle >= 0) { "maxIdle must be >= 0" }
        stateStore.setMaxIdle(config.poolName, maxIdle)
        currentMaxIdle = maxIdle
    }

    /**
     * Takes all idle sandbox IDs from the store and terminates each sandbox (best-effort).
     * Use this to release held resources, e.g. before process exit on single-node, or to reset the idle buffer.
     * In distributed mode this is best-effort: concurrent putIdle on other nodes may add new idle during the loop.
     * For a distributed idle drain, prefer [resize] to 0 and wait for snapshots to converge before using this
     * method as a final cleanup pass.
     * If the pool is not running, a temporary [SandboxManager] is created on demand so remote idle sandboxes can
     * still be killed. Failure to create that manager does not prevent draining idle IDs from the store.
     *
     * @return Number of idle sandboxes that were taken from the store and scheduled for best-effort kill.
     */
    fun releaseAllIdle(): Int {
        val poolName = config.poolName
        var count = 0
        var temporaryManager: SandboxManager? = null
        var killUnavailableLogged = false
        try {
            while (true) {
                val sandboxId = stateStore.tryTakeIdle(poolName) ?: break
                count++
                try {
                    val manager =
                        sandboxManager ?: temporaryManager ?: try {
                            createSandboxManager().also { temporaryManager = it }
                        } catch (e: Exception) {
                            if (!killUnavailableLogged) {
                                logger.warn(
                                    "releaseAllIdle: failed to create sandbox manager; draining idle ids without remote kill: " +
                                        "pool_name={} error={}",
                                    poolName,
                                    e.message,
                                )
                                killUnavailableLogged = true
                            }
                            null
                        }
                    if (manager == null) {
                        continue
                    }
                    manager.killSandbox(sandboxId)
                } catch (e: Exception) {
                    logger.warn(
                        "releaseAllIdle: failed to kill sandbox (best-effort): pool_name={} sandbox_id={} error={}",
                        poolName,
                        sandboxId,
                        e.message,
                    )
                }
            }
        } finally {
            temporaryManager?.close()
        }
        if (count > 0) {
            logger.info("releaseAllIdle: released {} idle sandbox(es): pool_name={}", count, poolName)
        }
        return count
    }

    /**
     * Returns a point-in-time snapshot of pool state for observability.
     */
    fun snapshot(): PoolSnapshot {
        val lifecycleState = lifecycleState.get()
        val state =
            when (lifecycleState) {
                LifecycleState.NOT_STARTED,
                LifecycleState.STOPPED,
                -> PoolState.STOPPED
                LifecycleState.DRAINING -> PoolState.DRAINING
                else -> reconcileState.state
            }
        val counters = stateStore.snapshotCounters(config.poolName)
        return PoolSnapshot(
            state = state,
            lifecycleState = lifecycleState.toPublicState(),
            idleCount = counters.idleCount,
            maxIdle = resolveMaxIdle(),
            failureCount = reconcileState.failureCount,
            backoffActive = reconcileState.isBackoffActive(),
            lastError = reconcileState.lastError,
            inFlightOperations = inFlightOperations.get(),
        )
    }

    /**
     * Returns a point-in-time snapshot of idle entries visible from the backing state store for this pool.
     */
    fun snapshotIdleEntries(): List<IdleEntry> {
        return stateStore.snapshotIdleEntries(config.poolName)
    }

    /**
     * Stops pool replenish workers. If [graceful] is true, transitions to DRAINING, stops reconcile worker,
     * and waits until local in-flight operations complete or [PoolConfig.drainTimeout] elapses before STOPPED.
     * acquire() is rejected while pool is not RUNNING. If [graceful] is false, stops immediately.
     */
    @Synchronized
    fun shutdown(graceful: Boolean = true) {
        if (lifecycleState.get() == LifecycleState.STOPPED) return
        if (!graceful) {
            stopReconcile()
            lifecycleState.set(LifecycleState.STOPPED)
            closeProvider()
            logger.info("Pool stopped (non-graceful): pool_name={} state={}", config.poolName, LifecycleState.STOPPED)
            return
        }
        lifecycleState.set(LifecycleState.DRAINING)
        stopReconcile()
        try {
            val drained = awaitInFlightDrain(config.drainTimeout)
            if (!drained) {
                logger.warn(
                    "Pool graceful shutdown timed out waiting in-flight operations: pool_name={} in_flight={} timeout_ms={}",
                    config.poolName,
                    inFlightOperations.get(),
                    config.drainTimeout.toMillis(),
                )
            }
        } catch (_: InterruptedException) {
            Thread.currentThread().interrupt()
            logger.warn("Pool graceful shutdown interrupted during drain: pool_name={}", config.poolName)
        } finally {
            lifecycleState.set(LifecycleState.STOPPED)
            closeProvider()
            logger.info("Pool stopped (graceful): pool_name={} state={}", config.poolName, LifecycleState.STOPPED)
        }
    }

    private fun resolveMaxIdle(): Int = stateStore.getMaxIdle(config.poolName) ?: currentMaxIdle

    private fun createSandboxManager(): SandboxManager = sandboxManagerFactory(connectionConfig.copyWithoutConnectionPool())

    private fun runReconcileTick() {
        if (lifecycleState.get() != LifecycleState.RUNNING) return
        val executor = warmupExecutor ?: return
        beginOperation()
        try {
            if (lifecycleState.get() != LifecycleState.RUNNING) return
            val reconcileConfig = config.withMaxIdle(resolveMaxIdle())
            PoolReconciler.runReconcileTick(
                config = reconcileConfig,
                stateStore = stateStore,
                createOne = { createOneSandbox() },
                onDiscardSandbox = { sandboxId -> killSandboxBestEffort(sandboxId) },
                reconcileState = reconcileState,
                warmupExecutor = executor,
            )
        } finally {
            endOperation()
        }
    }

    /**
     * Creates one sandbox via [Sandbox.builder], waits for readiness (no skipHealthCheck),
     * then returns its id. Caller must put the id into the store; the created [Sandbox]
     * is closed immediately so only the id is kept in the pool.
     */
    private fun createOneSandbox(): String? {
        beginOperation()
        return try {
            val sandbox = buildSandboxFromSpec()
            try {
                config.warmupSandboxPreparer?.prepare(sandbox)
                sandbox.id
            } catch (e: Exception) {
                try {
                    sandbox.kill()
                } catch (cleanupEx: Exception) {
                    logger.warn(
                        "Pool warmup sandbox preparer cleanup failed: pool_name={} sandbox_id={} error={}",
                        config.poolName,
                        sandbox.id,
                        cleanupEx.message,
                    )
                    e.addSuppressed(cleanupEx)
                }
                throw e
            } finally {
                sandbox.close()
            }
        } catch (e: Exception) {
            logger.warn("Pool create sandbox failed: poolName={}", config.poolName, e)
            throw e
        } finally {
            endOperation()
        }
    }

    private fun buildSandboxFromSpec(): Sandbox {
        val builder =
            creationSpec.applyToBuilder(
                Sandbox.builder()
                    .timeout(config.idleTimeout)
                    .readyTimeout(config.warmupReadyTimeout)
                    .healthCheckPollingInterval(config.warmupHealthCheckPollingInterval)
                    .skipHealthCheck(config.warmupSkipHealthCheck)
                    .connectionConfig(connectionConfig),
            )
        config.warmupHealthCheck?.let { builder.healthCheck(it) }
        return builder.build()
    }

    private fun directCreate(sandboxTimeout: Duration?): Sandbox {
        val builder =
            creationSpec.applyToBuilder(
                Sandbox.builder()
                    .timeout(config.idleTimeout)
                    .readyTimeout(config.acquireReadyTimeout)
                    .healthCheckPollingInterval(config.acquireHealthCheckPollingInterval)
                    .skipHealthCheck(config.acquireSkipHealthCheck)
                    .connectionConfig(connectionConfig),
            )
        config.acquireHealthCheck?.let { builder.healthCheck(it) }
        val sandbox = builder.build()
        sandboxTimeout?.let { sandbox.renew(it) }
        return sandbox
    }

    private fun killSandboxBestEffort(sandboxId: String) {
        try {
            sandboxManager?.killSandbox(sandboxId)
        } catch (e: Exception) {
            logger.warn(
                "Pool orphaned sandbox cleanup failed (best-effort): pool_name={} sandbox_id={} error={}",
                config.poolName,
                sandboxId,
                e.message,
            )
        }
    }

    private fun beginOperation() {
        inFlightOperations.incrementAndGet()
    }

    private fun endOperation() {
        val remaining = inFlightOperations.decrementAndGet()
        if (remaining < 0) {
            inFlightOperations.set(0)
            logger.warn("Pool in-flight counter underflow corrected: pool_name={}", config.poolName)
            inFlightLock.lock()
            try {
                inFlightZero.signalAll()
            } finally {
                inFlightLock.unlock()
            }
            return
        }
        if (remaining == 0) {
            inFlightLock.lock()
            try {
                inFlightZero.signalAll()
            } finally {
                inFlightLock.unlock()
            }
        }
    }

    @Throws(InterruptedException::class)
    private fun awaitInFlightDrain(timeout: Duration): Boolean {
        val timeoutNanos = timeout.toNanos()
        if (timeoutNanos <= 0) {
            return inFlightOperations.get() == 0
        }
        val deadline = System.nanoTime() + timeoutNanos
        inFlightLock.lock()
        try {
            while (inFlightOperations.get() > 0) {
                val remaining = deadline - System.nanoTime()
                if (remaining <= 0) {
                    return false
                }
                inFlightZero.awaitNanos(remaining)
            }
            return true
        } finally {
            inFlightLock.unlock()
        }
    }

    private fun stopReconcile() {
        reconcileTask?.cancel(true)
        reconcileTask = null
        scheduler?.let { shutdownExecutor(it, "scheduler") }
        scheduler = null
        warmupExecutor?.let { shutdownExecutor(it, "warmup") }
        warmupExecutor = null
        releasePrimaryLockBestEffort()
    }

    private fun releasePrimaryLockBestEffort() {
        try {
            stateStore.releasePrimaryLock(config.poolName, config.ownerId)
        } catch (e: Exception) {
            logger.warn(
                "Pool primary lock release failed (best-effort): pool_name={} owner_id={} error={}",
                config.poolName,
                config.ownerId,
                e.message,
            )
        }
    }

    private fun shutdownExecutor(
        executor: ExecutorService,
        role: String,
    ) {
        executor.shutdown()
        try {
            if (executor.awaitTermination(5, TimeUnit.SECONDS)) return
            val dropped = executor.shutdownNow()
            if (!executor.awaitTermination(5, TimeUnit.SECONDS)) {
                logger.warn(
                    "Pool {} executor did not terminate after forced stop: pool_name={} dropped_tasks={}",
                    role,
                    config.poolName,
                    dropped.size,
                )
            }
        } catch (_: InterruptedException) {
            val dropped = executor.shutdownNow()
            Thread.currentThread().interrupt()
            logger.warn(
                "Pool {} executor shutdown interrupted; forced stop issued: pool_name={} dropped_tasks={}",
                role,
                config.poolName,
                dropped.size,
            )
        }
    }

    private fun closeProvider() {
        try {
            sandboxManager?.close()
        } catch (e: Exception) {
            logger.warn("Error closing pool SandboxManager", e)
        }
        sandboxManager = null
    }

    @Suppress("ktlint:standard:property-naming")
    private enum class LifecycleState {
        NOT_STARTED,
        STARTING,
        RUNNING,
        DRAINING,
        STOPPED,
        ;

        fun toPublicState(): PoolLifecycleState =
            when (this) {
                NOT_STARTED -> PoolLifecycleState.NOT_STARTED
                STARTING -> PoolLifecycleState.STARTING
                RUNNING -> PoolLifecycleState.RUNNING
                DRAINING -> PoolLifecycleState.DRAINING
                STOPPED -> PoolLifecycleState.STOPPED
            }
    }

    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder internal constructor() {
        private var config: PoolConfig? = null

        fun config(config: PoolConfig): Builder {
            this.config = config
            return this
        }

        fun poolName(poolName: String): Builder {
            configBuilder.poolName(poolName)
            return this
        }

        fun ownerId(ownerId: String): Builder {
            configBuilder.ownerId(ownerId)
            return this
        }

        fun maxIdle(maxIdle: Int): Builder {
            configBuilder.maxIdle(maxIdle)
            return this
        }

        fun stateStore(stateStore: PoolStateStore): Builder {
            configBuilder.stateStore(stateStore)
            return this
        }

        fun connectionConfig(connectionConfig: ConnectionConfig): Builder {
            configBuilder.connectionConfig(connectionConfig)
            return this
        }

        fun creationSpec(creationSpec: PoolCreationSpec): Builder {
            configBuilder.creationSpec(creationSpec)
            return this
        }

        fun warmupConcurrency(warmupConcurrency: Int): Builder {
            configBuilder.warmupConcurrency(warmupConcurrency)
            return this
        }

        fun primaryLockTtl(primaryLockTtl: Duration): Builder {
            configBuilder.primaryLockTtl(primaryLockTtl)
            return this
        }

        fun reconcileInterval(reconcileInterval: Duration): Builder {
            configBuilder.reconcileInterval(reconcileInterval)
            return this
        }

        fun degradedThreshold(degradedThreshold: Int): Builder {
            configBuilder.degradedThreshold(degradedThreshold)
            return this
        }

        fun acquireReadyTimeout(acquireReadyTimeout: Duration): Builder {
            configBuilder.acquireReadyTimeout(acquireReadyTimeout)
            return this
        }

        fun acquireHealthCheckPollingInterval(acquireHealthCheckPollingInterval: Duration): Builder {
            configBuilder.acquireHealthCheckPollingInterval(acquireHealthCheckPollingInterval)
            return this
        }

        fun acquireHealthCheck(acquireHealthCheck: (Sandbox) -> Boolean): Builder {
            configBuilder.acquireHealthCheck(acquireHealthCheck)
            return this
        }

        fun acquireSkipHealthCheck(acquireSkipHealthCheck: Boolean = true): Builder {
            configBuilder.acquireSkipHealthCheck(acquireSkipHealthCheck)
            return this
        }

        fun warmupReadyTimeout(warmupReadyTimeout: Duration): Builder {
            configBuilder.warmupReadyTimeout(warmupReadyTimeout)
            return this
        }

        fun warmupHealthCheckPollingInterval(warmupHealthCheckPollingInterval: Duration): Builder {
            configBuilder.warmupHealthCheckPollingInterval(warmupHealthCheckPollingInterval)
            return this
        }

        fun warmupHealthCheck(warmupHealthCheck: (Sandbox) -> Boolean): Builder {
            configBuilder.warmupHealthCheck(warmupHealthCheck)
            return this
        }

        fun warmupSandboxPreparer(warmupSandboxPreparer: SandboxPreparer): Builder {
            configBuilder.warmupSandboxPreparer(warmupSandboxPreparer)
            return this
        }

        fun warmupSkipHealthCheck(warmupSkipHealthCheck: Boolean = true): Builder {
            configBuilder.warmupSkipHealthCheck(warmupSkipHealthCheck)
            return this
        }

        fun drainTimeout(drainTimeout: Duration): Builder {
            configBuilder.drainTimeout(drainTimeout)
            return this
        }

        fun idleTimeout(idleTimeout: Duration): Builder {
            configBuilder.idleTimeout(idleTimeout)
            return this
        }

        private val configBuilder = PoolConfig.builder()

        fun build(): SandboxPool {
            val cfg = config ?: configBuilder.build()
            return SandboxPool(cfg)
        }
    }
}

internal fun PoolCreationSpec.applyToBuilder(builder: Sandbox.Builder): Sandbox.Builder {
    val configuredBuilder =
        builder
            .imageSpec(imageSpec)
            .entrypoint(entrypoint)
            .resource(resource)
            .env(env)
            .metadata(metadata)
            .extensions(extensions)
            .volumes(volumes ?: emptyList())
            .secureAccess(secureAccess)

    networkPolicy?.let { configuredBuilder.networkPolicy(it) }
    platform?.let { configuredBuilder.platform(it) }
    return configuredBuilder
}
