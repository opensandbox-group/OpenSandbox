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
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolDestroyedException
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolEmptyException
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolNotRunningException
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolStateStoreUnavailableException
import com.alibaba.opensandbox.sandbox.domain.pool.AcquirePolicy
import com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry
import com.alibaba.opensandbox.sandbox.domain.pool.PoolConfig
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec
import com.alibaba.opensandbox.sandbox.domain.pool.PoolDestroyState
import com.alibaba.opensandbox.sandbox.domain.pool.PoolLifecycleState
import com.alibaba.opensandbox.sandbox.domain.pool.PoolSnapshot
import com.alibaba.opensandbox.sandbox.domain.pool.PoolState
import com.alibaba.opensandbox.sandbox.domain.pool.PoolStateStore
import com.alibaba.opensandbox.sandbox.domain.pool.PooledSandboxCreateContext
import com.alibaba.opensandbox.sandbox.domain.pool.PooledSandboxCreator
import com.alibaba.opensandbox.sandbox.domain.pool.SandboxPreparer
import com.alibaba.opensandbox.sandbox.infrastructure.pool.PoolReconciler
import com.alibaba.opensandbox.sandbox.infrastructure.pool.ReconcileState
import com.alibaba.opensandbox.sandbox.internal.isCausedByInterruption
import org.slf4j.LoggerFactory
import java.time.Duration
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicLong
import java.util.concurrent.atomic.AtomicReference
import java.util.concurrent.locks.Condition
import java.util.concurrent.locks.ReentrantLock
import kotlin.concurrent.withLock

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
    private val sandboxManagerFactory: (ConnectionConfig) -> SandboxManager,
    private val idleSandboxConnector: (String) -> Sandbox,
) {
    internal constructor(
        config: PoolConfig,
        sandboxManagerFactory: (ConnectionConfig) -> SandboxManager = { cfg ->
            SandboxManager.builder().connectionConfig(cfg).build()
        },
    ) : this(
        config = config,
        sandboxManagerFactory = sandboxManagerFactory,
        idleSandboxConnector = defaultIdleSandboxConnector(config),
    )

    private val logger = LoggerFactory.getLogger(SandboxPool::class.java)

    private val config: PoolConfig = config
    private val stateStore: PoolStateStore = config.stateStore
    private val connectionConfig: ConnectionConfig = config.connectionConfig
    private val creationSpec: PoolCreationSpec = config.creationSpec
    private val sandboxCreator: PooledSandboxCreator? = config.sandboxCreator
    private val reconcileState = ReconcileState(config.degradedThreshold)

    @Volatile
    private var currentMaxIdle: Int = config.maxIdle

    private val lifecycleState = AtomicReference(LifecycleState.NOT_STARTED)
    private var sandboxManager: SandboxManager? = null
    private var scheduler: ScheduledExecutorService? = null
    private var warmupExecutor: ExecutorService? = null
    private var reconcileTask: ScheduledFuture<*>? = null
    private var primaryHeartbeatTask: ScheduledFuture<*>? = null
    private val runSequence = AtomicLong(0)

    @Volatile
    private var currentRun: RunContext? = null

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
            ensurePoolNamespaceActive()
            sandboxManager = createSandboxManager()
            stateStore.setIdleEntryTtl(config.poolName, config.idleTimeout)
            stateStore.setMaxIdle(config.poolName, config.maxIdle)
            val warmupExec =
                Executors.newFixedThreadPool(config.warmupConcurrency.coerceAtLeast(1)) { r ->
                    Thread(r, "sandbox-pool-warmup-${config.poolName}").apply { isDaemon = true }
                }
            warmupExecutor = warmupExec
            val exec =
                Executors.newSingleThreadScheduledExecutor { r ->
                    Thread(r, "sandbox-pool-reconcile-${config.poolName}").apply { isDaemon = true }
                }
            scheduler = exec
            val run = RunContext(runSequence.incrementAndGet(), exec, warmupExec)
            currentRun = run
            val reconcileIntervalMs = config.reconcileInterval.toMillis()
            val primaryHeartbeatIntervalMs =
                minOf(
                    reconcileIntervalMs,
                    config.primaryLockTtl.dividedBy(3L).toMillis(),
                ).coerceAtLeast(1L)
            // The first reconcile may run immediately. Publish RUNNING only after all of its
            // dependencies are initialized, but before scheduling it so that tick is not skipped.
            lifecycleState.set(LifecycleState.RUNNING)
            primaryHeartbeatTask =
                exec.scheduleAtFixedRate(
                    {
                        try {
                            runPrimaryHeartbeat(run)
                        } catch (t: Throwable) {
                            // Keep periodic heartbeats alive after transient store failures.
                            logger.error("Pool primary heartbeat failed: pool_name={}", config.poolName, t)
                        }
                    },
                    primaryHeartbeatIntervalMs,
                    primaryHeartbeatIntervalMs,
                    TimeUnit.MILLISECONDS,
                )
            reconcileTask =
                exec.scheduleAtFixedRate(
                    {
                        try {
                            runReconcileTick(run)
                        } catch (t: Throwable) {
                            // Keep periodic scheduling alive even if one tick fails unexpectedly.
                            logger.error("Pool reconcile tick failed unexpectedly: pool_name={}", config.poolName, t)
                        }
                    },
                    if (config.maxIdle > 0) 0 else reconcileIntervalMs,
                    reconcileIntervalMs,
                    TimeUnit.MILLISECONDS,
                )
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

    /**
     * Acquires a sandbox from the pool or creates one directly per policy.
     *
     * 1. Tries to take an idle sandbox ID from the store and connect.
     * 2. If connect fails (stale ID), removes the ID, best-effort kill, then applies the policy:
     *    - [AcquirePolicy.FAIL_FAST] / [AcquirePolicy.DIRECT_CREATE]: no retry across idles;
     *      FAIL_FAST throws, DIRECT_CREATE falls through to lifecycle create.
     *    - [AcquirePolicy.RETRY_NEXT_IDLE] / [AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE]:
     *      skip the bad candidate and try the next idle up to [PoolConfig.maxAcquireRetries]
     *      total attempts. On exhaustion, `_THEN_CREATE` falls through to lifecycle create;
     *      the retry-only variant throws [PoolAcquireFailedException].
     * 3. Under [AcquirePolicy.FAIL_FAST] / [AcquirePolicy.RETRY_NEXT_IDLE]:
     *    - throws [PoolEmptyException] when idle buffer is empty (no candidate ever seen);
     *    - throws [PoolAcquireFailedException] with `cause` set to the last connect failure
     *      when at least one idle candidate was attempted.
     * 4. If pool is not RUNNING (e.g. DRAINING/STOPPED), throws [PoolNotRunningException].
     *
     * @param sandboxTimeout Optional duration to set on the acquired sandbox (applied via renew after connect).
     * @param policy Behavior on idle-empty / candidate failure (default: [AcquirePolicy.DIRECT_CREATE]).
     * @return A connected [Sandbox] instance. Caller must call [Sandbox.kill] when done.
     * @throws PoolNotRunningException when pool lifecycle state is not RUNNING.
     * @throws PoolEmptyException when the effective policy throws and idle was empty.
     * @throws PoolAcquireFailedException when the effective policy throws and an idle candidate was attempted.
     * @throws SandboxException for lifecycle create/connect/renew errors.
     */
    fun acquire(
        sandboxTimeout: Duration? = null,
        policy: AcquirePolicy = AcquirePolicy.DIRECT_CREATE,
    ): Sandbox {
        if (lifecycleState.get() != LifecycleState.RUNNING) {
            val state = lifecycleState.get()
            throwIfPoolNamespaceDestroyed()
            logger.info("Pool not running, acquire rejected: pool_name={} state={}", config.poolName, state)
            throw PoolNotRunningException("Cannot acquire when pool state is $state")
        }
        val run = currentRun ?: throw PoolNotRunningException("Cannot acquire without an active pool run")
        val poolName = config.poolName
        val pendingKill = ArrayList<String>()
        beginOperation(run)
        try {
            ensureAcquireRunActive(run)
            ensurePoolNamespaceActiveForAcquire(policy)
            val maxAttempts = effectiveMaxIdleAttempts(policy)
            // Accumulate discarded-alive sandbox IDs across all take iterations so we schedule
            // a single deferred cleanup, instead of one kill batch per retry.
            var lastSandboxId: String? = null
            var lastIdleConnectFailure: Exception? = null
            var attemptedAny = false
            var attempt = 0
            var loopExhausted = true
            while (attempt < maxAttempts) {
                attempt++
                val takeResult =
                    try {
                        // Linearize the active-run fence with the destructive take against retireRun.
                        // Otherwise an old acquire could pop an idle committed by a restarted run.
                        run.commitLock.withLock {
                            ensureAcquireRunActive(run)
                            stateStore.tryTakeIdle(poolName, config.acquireMinRemainingTtl)
                        }
                    } catch (e: PoolStateStoreUnavailableException) {
                        // State store outage. Per OSEP-0005, under policies that fall through to
                        // direct-create on empty idle we degrade to that fallback so the pool
                        // stays at least as available as raw SDK usage during store outages.
                        // Under non-fallthrough policies (FAIL_FAST / RETRY_NEXT_IDLE) we surface
                        // the outage as-is so callers can react.
                        if (!policyFallsThroughToDirectCreate(policy)) {
                            throw e
                        }
                        ensureAcquireRunActive(run)
                        logger.warn(
                            "Acquire: state store unavailable, falling through to direct create " +
                                "per policy={} error={}",
                            policy,
                            e.message,
                        )
                        loopExhausted = false
                        break
                    }
                if (takeResult.discardedAliveSandboxIds.isNotEmpty()) {
                    pendingKill.addAll(takeResult.discardedAliveSandboxIds)
                }
                val sandboxId = takeResult.sandboxId
                if (sandboxId == null) {
                    // Idle buffer drained mid-loop (or was empty from the start). Stop retrying —
                    // continuing would just pay another take round-trip for no gain.
                    loopExhausted = false
                    break
                }
                lastSandboxId = sandboxId
                attemptedAny = true
                val sandbox: Sandbox
                try {
                    sandbox = idleSandboxConnector(sandboxId)
                } catch (failure: Throwable) {
                    val restoreInterrupt = Thread.interrupted() || failure.isCausedByInterruption()
                    try {
                        discardAcquireCandidate(run, sandboxId, failure)
                        if (failure is PoolDestroyedException) throw failure
                        if (restoreInterrupt || failure !is Exception) throw failure

                        lastIdleConnectFailure = failure
                        logger.warn(
                            "Idle connect failed (stale or unreachable), removed from pool: " +
                                "pool_name={} sandbox_id={} policy={} attempt={}/{} error={}",
                            poolName,
                            sandboxId,
                            policy,
                            attempt,
                            maxAttempts,
                            failure.message,
                        )
                        ensureAcquireRunActive(run)
                        ensurePoolNamespaceActive()
                        continue
                    } finally {
                        if (restoreInterrupt) {
                            Thread.currentThread().interrupt()
                        }
                    }
                }
                // Connect + readiness succeeded. From here on the sandbox is a healthy, borrowable
                // idle: renew and namespace failures are operation-wide and must not consume another
                // candidate. Dispose the popped sandbox before propagating either failure.
                try {
                    sandboxTimeout?.let { sandbox.renew(it) }
                } catch (failure: Throwable) {
                    disposeSandboxAfterAcquireFailure(sandbox, failure)
                    throw failure
                }
                ensurePoolNamespaceActiveOrDispose(sandbox)
                ensureAcquireRunActive(run, sandbox)
                logger.debug(
                    "Acquire from idle: pool_name={} sandbox_id={} policy={} attempt={}/{}",
                    poolName,
                    sandboxId,
                    policy,
                    attempt,
                    maxAttempts,
                )
                return sandbox
            }

            val reason =
                when {
                    !attemptedAny -> "idle buffer empty"
                    loopExhausted ->
                        "idle connect failed for $maxAttempts candidate(s); last sandbox_id=$lastSandboxId " +
                            "(stale or unreachable)"
                    else ->
                        "idle connect failed for sandbox_id=$lastSandboxId; idle buffer drained " +
                            "before reaching maxAcquireRetries=$maxAttempts"
                }
            if (!policyFallsThroughToDirectCreate(policy)) {
                logger.debug("Acquire no-fallback: pool_name={} policy={} reason={}", poolName, policy, reason)
                if (attemptedAny) {
                    throw PoolAcquireFailedException(
                        message = "Cannot acquire: $reason; policy is $policy",
                        cause = lastIdleConnectFailure,
                    )
                }
                throw PoolEmptyException("Cannot acquire: $reason; policy is $policy")
            }
            ensureAcquireRunActive(run)
            ensurePoolNamespaceActiveForAcquire(policy)
            logger.debug("Acquire direct create: pool_name={} reason={} policy={}", poolName, reason, policy)
            val sandbox = directCreate(sandboxTimeout, policy)
            ensureAcquireRunActive(run, sandbox)
            return sandbox
        } finally {
            val restoreInterrupt = Thread.interrupted()
            try {
                scheduleAcquireCleanup(run, pendingKill, source = "acquire")
            } finally {
                try {
                    endOperation(run)
                } finally {
                    if (restoreInterrupt) {
                        Thread.currentThread().interrupt()
                    }
                }
            }
        }
    }

    /**
     * Effective per-acquire cap on how many idle candidates to attempt before giving up. The
     * legacy single-shot policies always try exactly one idle; the retry policies use the
     * user-configured `maxAcquireRetries` (default 3). Kept private so we can revisit the bound
     * (e.g. add a wall-clock deadline) without changing the public policy enum.
     */
    private fun effectiveMaxIdleAttempts(policy: AcquirePolicy): Int =
        when (policy) {
            AcquirePolicy.FAIL_FAST, AcquirePolicy.DIRECT_CREATE -> 1
            AcquirePolicy.RETRY_NEXT_IDLE, AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE ->
                config.maxAcquireRetries.coerceAtLeast(1)
        }

    /**
     * Returns whether [policy], after exhausting its idle budget (or on state-store outage),
     * should silently create a fresh sandbox instead of throwing. Mirrors the equivalent Go /
     * Python helpers so the three SDKs share one fallthrough classification.
     */
    private fun policyFallsThroughToDirectCreate(policy: AcquirePolicy): Boolean =
        policy == AcquirePolicy.DIRECT_CREATE || policy == AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE

    /**
     * Updates the maximum idle target. In distributed mode the new value is written to the store
     * so the whole cluster (including the leader) uses it; in single-node only this process sees it.
     * This method can be called from any node. Actual replenish or shrink work is performed
     * asynchronously by the current primary during periodic reconcile.
     */
    fun resize(maxIdle: Int) {
        require(maxIdle >= 0) { "maxIdle must be >= 0" }
        ensurePoolNamespaceActive()
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
     * Takes all idle sandbox IDs from the store and terminates them with bounded concurrency.
     * This method blocks until every ID taken from the store has received a best-effort kill attempt.
     *
     * @param concurrency Maximum number of concurrent kill requests. Must be positive.
     * @return Number of idle sandboxes taken from the store.
     */
    fun releaseAllIdle(concurrency: Int): Int {
        require(concurrency > 0) { "concurrency must be positive" }
        val poolName = config.poolName
        val sandboxIds = mutableListOf<String>()
        var drainFailure: Exception? = null
        var temporaryManager: SandboxManager? = null
        try {
            while (true) {
                val sandboxId =
                    try {
                        stateStore.tryTakeIdle(poolName)
                    } catch (e: Exception) {
                        drainFailure = e
                        break
                    } ?: break
                sandboxIds.add(sandboxId)
            }

            if (sandboxIds.isNotEmpty()) {
                val manager =
                    sandboxManager ?: try {
                        createSandboxManager().also { temporaryManager = it }
                    } catch (e: Exception) {
                        logger.warn(
                            "releaseAllIdle(concurrency): failed to create sandbox manager; " +
                                "draining idle ids without remote kill: " +
                                "pool_name={} error={}",
                            poolName,
                            e.message,
                        )
                        null
                    }
                if (manager != null) {
                    val threadIndex = AtomicInteger()
                    val executor =
                        Executors.newFixedThreadPool(minOf(concurrency, sandboxIds.size)) { runnable ->
                            Thread(
                                runnable,
                                "sandbox-pool-release-$poolName-${threadIndex.incrementAndGet()}",
                            ).apply { isDaemon = true }
                        }
                    try {
                        sandboxIds.forEach { sandboxId ->
                            executor.submit {
                                try {
                                    manager.killSandbox(sandboxId)
                                } catch (e: Exception) {
                                    logger.warn(
                                        "releaseAllIdle(concurrency): failed to kill sandbox (best-effort): " +
                                            "pool_name={} sandbox_id={} error={}",
                                        poolName,
                                        sandboxId,
                                        e.message,
                                    )
                                }
                            }
                        }
                    } finally {
                        executor.shutdown()
                        var interrupted = false
                        while (!executor.isTerminated) {
                            try {
                                executor.awaitTermination(Long.MAX_VALUE, TimeUnit.NANOSECONDS)
                            } catch (_: InterruptedException) {
                                interrupted = true
                            }
                        }
                        if (interrupted) {
                            Thread.currentThread().interrupt()
                        }
                    }
                }
            }
        } finally {
            temporaryManager?.close()
        }
        drainFailure?.let { throw it }
        if (sandboxIds.isNotEmpty()) {
            logger.info(
                "releaseAllIdle(concurrency): released {} idle sandbox(es): pool_name={}",
                sandboxIds.size,
                poolName,
            )
        }
        return sandboxIds.size
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
            inFlightOperations = currentRun?.inFlightOperations?.get() ?: 0,
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
     * prevents future reconcile ticks without interrupting the current tick, and waits until local in-flight
     * operations complete or [PoolConfig.drainTimeout] elapses before STOPPED.
     * acquire() is rejected while pool is not RUNNING. If [graceful] is false, stops immediately.
     */
    @Synchronized
    fun shutdown(graceful: Boolean = true) {
        if (lifecycleState.get() == LifecycleState.STOPPED) return
        val run = currentRun
        run?.warmupSubmissionsOpen?.set(false)
        if (!graceful) {
            lifecycleState.set(LifecycleState.STOPPED)
            stopReconcile()
            closeProvider()
            logger.info("Pool stopped (non-graceful): pool_name={} state={}", config.poolName, LifecycleState.STOPPED)
            return
        }
        lifecycleState.set(LifecycleState.DRAINING)
        var drained = false
        try {
            beginGracefulReconcileStop()
            drained = awaitInFlightDrain(run, config.drainTimeout)
            if (!drained) {
                logger.warn(
                    "Pool graceful shutdown timed out waiting in-flight operations: pool_name={} in_flight={} timeout_ms={}",
                    config.poolName,
                    run?.inFlightOperations?.get() ?: 0,
                    config.drainTimeout.toMillis(),
                )
            }
        } catch (_: InterruptedException) {
            Thread.currentThread().interrupt()
            logger.warn("Pool graceful shutdown interrupted during drain: pool_name={}", config.poolName)
        } finally {
            if (drained) {
                completeGracefulReconcileStop()
            } else {
                lifecycleState.set(LifecycleState.STOPPED)
                forceStopReconcileAfterGracefulDrain()
            }
            lifecycleState.set(LifecycleState.STOPPED)
            closeProvider()
            logger.info("Pool stopped (graceful): pool_name={} state={}", config.poolName, LifecycleState.STOPPED)
        }
    }

    private fun resolveMaxIdle(): Int = stateStore.getMaxIdle(config.poolName) ?: currentMaxIdle

    private fun ensureAcquireRunActive(
        run: RunContext,
        sandbox: Sandbox? = null,
    ) {
        if (isCurrentRun(run) && lifecycleState.get() == LifecycleState.RUNNING) return

        val state = lifecycleState.get()
        val failure =
            try {
                throwIfPoolNamespaceDestroyed()
                PoolNotRunningException(
                    "Cannot acquire after pool run ${run.generation} was retired; current state is $state",
                )
            } catch (t: Throwable) {
                t
            }
        sandbox?.let { disposeSandboxAfterAcquireFailure(it, failure) }
        throw failure
    }

    private fun discardAcquireCandidate(
        run: RunContext,
        sandboxId: String,
        failure: Throwable,
    ) {
        try {
            stateStore.removeIdle(config.poolName, sandboxId)
        } catch (cleanupFailure: Throwable) {
            if (cleanupFailure !== failure) {
                failure.addSuppressed(cleanupFailure)
            }
        }
        scheduleAcquireCleanup(run, listOf(sandboxId), source = "acquire-stale")
    }

    private fun scheduleAcquireCleanup(
        run: RunContext,
        sandboxIds: List<String>,
        source: String,
    ) {
        scheduleKillDiscardedAlive(
            config.poolName,
            sandboxIds,
            source = source,
            executor = run.warmupExecutor,
            run = run,
        )
    }

    private fun disposeSandboxAfterAcquireFailure(
        sandbox: Sandbox,
        failure: Throwable,
    ) {
        val restoreInterrupt = Thread.interrupted() || failure.isCausedByInterruption()
        try {
            try {
                sandbox.kill()
            } catch (cleanupFailure: Throwable) {
                if (cleanupFailure !== failure) {
                    failure.addSuppressed(cleanupFailure)
                }
                logger.warn(
                    "Pool sandbox cleanup after acquire failure failed: " +
                        "pool_name={} sandbox_id={} operation=kill error={}",
                    config.poolName,
                    sandbox.id,
                    cleanupFailure.message,
                )
            }
            try {
                sandbox.close()
            } catch (cleanupFailure: Throwable) {
                if (cleanupFailure !== failure) {
                    failure.addSuppressed(cleanupFailure)
                }
                logger.warn(
                    "Pool sandbox cleanup after acquire failure failed: " +
                        "pool_name={} sandbox_id={} operation=close error={}",
                    config.poolName,
                    sandbox.id,
                    cleanupFailure.message,
                )
            }
        } finally {
            if (restoreInterrupt) {
                Thread.currentThread().interrupt()
            }
        }
    }

    /**
     * Offload [killDiscardedAlive] to the warmup executor so the caller does not block on the
     * kill RPCs. Falls back to inline execution when no executor is available (e.g. the pool is
     * shutting down) — better to slow the caller than to drop the cleanup entirely.
     */
    private fun scheduleKillDiscardedAlive(
        poolName: String,
        sandboxIds: List<String>,
        source: String,
        executor: ExecutorService? = warmupExecutor,
        run: RunContext? = currentRun,
    ) {
        if (sandboxIds.isEmpty()) return
        val cleanupTask = TrackedCleanupTask(poolName, sandboxIds.toList(), source, run)
        if (executor == null) {
            cleanupTask.run()
            return
        }
        try {
            // execute() deliberately preserves the task identity in ThreadPoolExecutor's queue.
            // shutdownNow() can then return this exact task so its drain count is completed even
            // when the cleanup never starts.
            executor.execute(cleanupTask)
        } catch (e: Exception) {
            // Executor may reject if the pool is mid-shutdown; fall back to inline kill.
            logger.debug(
                "Discarded-alive kill submit rejected, running inline: pool_name={} count={} error={}",
                poolName,
                sandboxIds.size,
                e.message,
            )
            cleanupTask.run()
        }
    }

    private inner class TrackedCleanupTask(
        private val poolName: String,
        private val sandboxIds: List<String>,
        private val source: String,
        private val run: RunContext?,
    ) : Runnable {
        private val completed = AtomicBoolean(false)

        init {
            run?.let { beginOperation(it) }
        }

        override fun run() {
            try {
                killDiscardedAlive(poolName, sandboxIds, source)
            } finally {
                complete()
            }
        }

        fun completeIfDropped() {
            complete()
        }

        private fun complete() {
            if (completed.compareAndSet(false, true)) {
                run?.let { endOperation(it) }
            }
        }
    }

    private fun completeDroppedTasks(dropped: List<Runnable>) {
        dropped.forEach { task ->
            when (task) {
                is TrackedCleanupTask -> task.completeIfDropped()
                is TrackedWarmupTask -> task.completeIfDropped()
                is TrackedWarmupCompletionTask -> task.completeIfDropped()
            }
        }
    }

    /**
     * Best-effort terminate sandboxes the store dropped because their remaining TTL fell below
     * `acquireMinRemainingTtl`. The store has already removed them from idle membership; without
     * this kill they would linger on the server until their TTL elapses, exceeding the intended
     * pool size during the gap.
     *
     * Failures are logged and swallowed: the caller's primary outcome (acquire/reconcile) must
     * not be impacted by a janitor failure.
     */
    private fun killDiscardedAlive(
        poolName: String,
        sandboxIds: List<String>,
        source: String,
    ) {
        if (sandboxIds.isEmpty()) return
        var temporaryManager: SandboxManager? = null
        val manager =
            sandboxManager ?: try {
                createSandboxManager().also { temporaryManager = it }
            } catch (e: Exception) {
                logger.warn(
                    "Failed to create manager for discarded sandbox cleanup: " +
                        "pool_name={} count={} source={} error={}",
                    poolName,
                    sandboxIds.size,
                    source,
                    e.message,
                )
                return
            }
        try {
            for (sandboxId in sandboxIds) {
                try {
                    manager.killSandbox(sandboxId)
                    logger.debug(
                        "Killed discarded sandbox: pool_name={} sandbox_id={} source={}",
                        poolName,
                        sandboxId,
                        source,
                    )
                } catch (e: Exception) {
                    logger.warn(
                        "Failed to kill discarded sandbox (best-effort, will expire server-side): " +
                            "pool_name={} sandbox_id={} source={} error={}",
                        poolName,
                        sandboxId,
                        source,
                        e.message,
                    )
                }
            }
        } finally {
            try {
                temporaryManager?.close()
            } catch (e: Exception) {
                logger.warn(
                    "Failed to close temporary manager after discarded sandbox cleanup: " +
                        "pool_name={} source={} error={}",
                    poolName,
                    source,
                    e.message,
                )
            }
        }
    }

    private fun createSandboxManager(): SandboxManager = sandboxManagerFactory(connectionConfig.copyWithoutConnectionPool())

    private fun runReconcileTick(run: RunContext) {
        if (!isCurrentRun(run) || lifecycleState.get() != LifecycleState.RUNNING) return
        if (!isPoolNamespaceActive()) {
            logger.info("Pool namespace is destroyed; stopping local pool: pool_name={}", config.poolName)
            stopAfterNamespaceDestroyed()
            return
        }
        beginOperation(run)
        try {
            if (!isCurrentRun(run) || lifecycleState.get() != LifecycleState.RUNNING) return
            if (!isPoolNamespaceActive()) return
            val reconcileConfig = config.withMaxIdle(resolveMaxIdle())
            try {
                run.primaryOwned.set(
                    PoolReconciler.runReconcileTick(
                        config = reconcileConfig,
                        stateStore = stateStore,
                        onDiscardSandbox = { sandboxId -> killSandboxBestEffort(sandboxId) },
                        reconcileState = reconcileState,
                        warmingCount = run.warmingCount.get(),
                        submitWarmups = { count -> submitWarmups(run, count) },
                    ),
                )
            } catch (e: Exception) {
                run.primaryOwned.set(false)
                throw e
            }
        } finally {
            endOperation(run)
        }
    }

    private fun runPrimaryHeartbeat(run: RunContext) {
        if (!isCurrentRun(run)) return
        val state = lifecycleState.get()
        if (state != LifecycleState.RUNNING && state != LifecycleState.DRAINING) return
        if (!run.primaryOwned.get()) return
        val renewed =
            stateStore.renewPrimaryLock(
                config.poolName,
                config.ownerId,
                config.primaryLockTtl,
            )
        if (!renewed) {
            run.primaryOwned.set(false)
            logger.trace(
                "Pool primary heartbeat skipped (not current owner): pool_name={} owner_id={}",
                config.poolName,
                config.ownerId,
            )
        }
    }

    private fun requestReconcile(run: RunContext) {
        if (!isCurrentRun(run) ||
            lifecycleState.get() != LifecycleState.RUNNING ||
            !run.warmupSubmissionsOpen.get()
        ) {
            return
        }
        val exec = run.scheduler
        if (!run.reconcileQueued.compareAndSet(false, true)) return
        // Completion-driven ticks are a latency optimization, not the correctness loop (the
        // periodic tick is). Coalesce them into a minimum interval so bursts of fast
        // completions cannot drive reconcile ticks — each of which costs several state-store
        // round-trips — at unbounded frequency. The floor advances from the later of the last
        // request and the previous floor so it applies between tick executions, not merely
        // between requests: a completion that lands right after a scheduled tick must still
        // wait out the full window.
        val nowNanos = System.nanoTime()
        val nextAllowedNanos = run.nextCompletionReconcileAtNanos
        run.nextCompletionReconcileAtNanos =
            maxOf(nextAllowedNanos, nowNanos) + TimeUnit.MILLISECONDS.toNanos(COMPLETION_RECONCILE_MIN_INTERVAL_MS)
        val delayMs = TimeUnit.NANOSECONDS.toMillis(nextAllowedNanos - nowNanos).coerceAtLeast(0L)
        val tick =
            Runnable {
                run.reconcileQueued.set(false)
                try {
                    runReconcileTick(run)
                } catch (t: Throwable) {
                    logger.error("Pool completion-driven reconcile failed: pool_name={}", config.poolName, t)
                }
            }
        try {
            if (delayMs == 0L) {
                exec.execute(tick)
            } else {
                exec.schedule(tick, delayMs, TimeUnit.MILLISECONDS)
            }
        } catch (e: Exception) {
            run.reconcileQueued.set(false)
            if (lifecycleState.get() == LifecycleState.RUNNING) {
                logger.debug(
                    "Pool completion-driven reconcile submit rejected: pool_name={} error={}",
                    config.poolName,
                    e.message,
                )
            }
        }
    }

    private fun submitWarmups(
        run: RunContext,
        count: Int,
    ) {
        repeat(count) {
            if (!isCurrentRun(run) ||
                lifecycleState.get() != LifecycleState.RUNNING ||
                !run.warmupSubmissionsOpen.get()
            ) {
                return
            }
            val task = TrackedWarmupTask(run)
            try {
                run.warmupExecutor.execute(task)
            } catch (e: Exception) {
                logger.debug(
                    "Pool warmup submit rejected: pool_name={} error={}",
                    config.poolName,
                    e.message,
                )
                task.completeIfDropped()
                return
            }
        }
    }

    private inner class TrackedWarmupTask(
        private val run: RunContext,
    ) : Runnable {
        private val completed = AtomicBoolean(false)

        init {
            run.warmingCount.incrementAndGet()
            beginOperation(run)
        }

        override fun run() {
            val outcome =
                try {
                    WarmupOutcome.Success(createOneSandbox())
                } catch (failure: Throwable) {
                    WarmupOutcome.Failure(failure)
                }
            dispatchCompletion(outcome)
        }

        fun completeIfDropped() {
            complete(WarmupOutcome.Cancelled)
        }

        private fun dispatchCompletion(outcome: WarmupOutcome) {
            val completion = TrackedWarmupCompletionTask(run, this, outcome)
            run.pendingWarmupCompletions.add(completion)
            try {
                run.scheduler.execute(completion)
            } catch (e: Exception) {
                logger.debug(
                    "Pool warmup completion submit rejected, handling inline: pool_name={} error={}",
                    config.poolName,
                    e.message,
                )
                completion.run()
            }
        }

        fun complete(outcome: WarmupOutcome) {
            if (!completed.compareAndSet(false, true)) return
            try {
                handleWarmupOutcome(run, outcome)
            } finally {
                run.warmingCount.decrementAndGet()
                endOperation(run)
                // Only successful completions trigger an immediate reconcile. A failed warmup
                // frees its slot but must not cause an immediate retry: fast-failing creates
                // would otherwise form a self-sustaining reconcile/create loop that amplifies
                // state-store load far beyond the periodic tick. Retries are driven by the
                // periodic tick, which the backoff window already paces.
                if (outcome is WarmupOutcome.Success) {
                    requestReconcile(run)
                }
            }
        }
    }

    /**
     * Tracks a warmup outcome while it waits in the controller queue.
     *
     * Scheduled executors may wrap submitted [Runnable]s before returning them from `shutdownNow()`,
     * so [RunContext.pendingWarmupCompletions] is the authoritative registry. Both normal execution
     * and forced shutdown finish the original warmup through this task; the warmup's CAS keeps that
     * completion exactly once.
     */
    private inner class TrackedWarmupCompletionTask(
        private val run: RunContext,
        private val warmupTask: TrackedWarmupTask,
        private val outcome: WarmupOutcome,
    ) : Runnable {
        override fun run() {
            try {
                warmupTask.complete(outcome)
            } finally {
                run.pendingWarmupCompletions.remove(this)
            }
        }

        fun completeIfDropped() {
            run()
        }
    }

    private fun completePendingWarmupCompletions(run: RunContext?) {
        run?.pendingWarmupCompletions?.toList()?.forEach { it.completeIfDropped() }
    }

    private sealed interface WarmupOutcome {
        class Success(
            val sandboxId: String,
        ) : WarmupOutcome

        class Failure(
            val error: Throwable,
        ) : WarmupOutcome

        data object Cancelled : WarmupOutcome
    }

    private fun handleWarmupOutcome(
        run: RunContext,
        outcome: WarmupOutcome,
    ) {
        when (outcome) {
            is WarmupOutcome.Success -> commitWarmupSandbox(run, outcome.sandboxId)
            is WarmupOutcome.Failure -> {
                if (isCurrentRun(run) && lifecycleState.get() == LifecycleState.RUNNING) {
                    reconcileState.recordAsyncFailure(outcome.error.message)
                }
            }
            WarmupOutcome.Cancelled -> Unit
        }
    }

    private fun commitWarmupSandbox(
        run: RunContext,
        sandboxId: String,
    ) {
        var cleanupSource: String? = null
        run.commitLock.lock()
        try {
            val state = lifecycleState.get()
            if (!isCurrentRun(run) || (state != LifecycleState.RUNNING && state != LifecycleState.DRAINING)) {
                cleanupSource = "warmup-stale-run"
            } else {
                try {
                    ensurePoolNamespaceActive()
                    if (!stateStore.renewPrimaryLock(config.poolName, config.ownerId, config.primaryLockTtl)) {
                        run.primaryOwned.set(false)
                        logger.warn(
                            "Pool lost primary lock before putIdle; dropping warmup sandbox: " +
                                "pool_name={} sandbox_id={} run={}",
                            config.poolName,
                            sandboxId,
                            run.generation,
                        )
                        cleanupSource = "warmup-lock-lost"
                    } else {
                        stateStore.putIdle(config.poolName, sandboxId)
                        reconcileState.recordSuccess()
                        logger.debug(
                            "Pool warmup sandbox entered idle: pool_name={} sandbox_id={} run={}",
                            config.poolName,
                            sandboxId,
                            run.generation,
                        )
                    }
                } catch (e: Exception) {
                    if (isCurrentRun(run) && lifecycleState.get() == LifecycleState.RUNNING) {
                        reconcileState.recordAsyncFailure(e.message)
                    }
                    try {
                        stateStore.removeIdle(config.poolName, sandboxId)
                    } catch (_: Exception) {
                        // best-effort remove before remote cleanup
                    }
                    cleanupSource = "warmup-commit-failed"
                    logger.warn(
                        "Pool warmup commit failed; dropped sandbox: pool_name={} sandbox_id={} run={} error={}",
                        config.poolName,
                        sandboxId,
                        run.generation,
                        e.message,
                    )
                }
            }
        } finally {
            run.commitLock.unlock()
        }
        cleanupSource?.let { source ->
            scheduleKillDiscardedAlive(
                config.poolName,
                listOf(sandboxId),
                source = source,
                executor = run.warmupExecutor,
                run = run,
            )
        }
    }

    /**
     * Creates one sandbox, waits for readiness, then returns its id. Caller must put the
     * id into the store; the created [Sandbox] is closed immediately so only the id is
     * kept in the pool.
     */
    private fun createOneSandbox(): String {
        return try {
            val sandbox = buildWarmupSandbox()
            var failure: Throwable? = null
            try {
                config.warmupSandboxPreparer?.prepare(sandbox)
                // The server-side TTL has been ticking since sandbox creation; readiness
                // wait and `warmupSandboxPreparer` can both consume meaningful time (think
                // initialization scripts). Renew right before handing the id back to the
                // reconciler so the store's stamped expiry (now + idleTimeout) actually matches
                // what the server will honor — otherwise `acquireMinRemainingTtl` overestimates
                // remaining TTL by the warmup duration.
                sandbox.renew(config.idleTimeout)
                sandbox.id
            } catch (t: Throwable) {
                failure = t
                killWarmupSandboxAfterFailure(sandbox, t)
                throw t
            } finally {
                try {
                    sandbox.close()
                } catch (closeFailure: Throwable) {
                    if (failure != null) {
                        if (closeFailure !== failure) {
                            failure.addSuppressed(closeFailure)
                        }
                    } else {
                        killWarmupSandboxAfterFailure(sandbox, closeFailure)
                        throw closeFailure
                    }
                }
            }
        } catch (failure: Throwable) {
            logger.warn("Pool create sandbox failed: poolName={}", config.poolName, failure)
            throw failure
        }
    }

    private fun killWarmupSandboxAfterFailure(
        sandbox: Sandbox,
        failure: Throwable,
    ) {
        // A warmup preparer may restore the thread's interrupted status before propagating an
        // InterruptedException. OkHttp treats that status as cancellation and can reject the
        // cleanup request before the DELETE is sent. Temporarily clear it for the synchronous
        // cleanup, then restore it so executor shutdown semantics are preserved.
        val restoreInterrupt = Thread.interrupted() || failure.isCausedByInterruption()
        try {
            sandbox.kill()
        } catch (cleanupFailure: Throwable) {
            logger.warn(
                "Pool warmup sandbox preparer cleanup failed: pool_name={} sandbox_id={} error={}",
                config.poolName,
                sandbox.id,
                cleanupFailure.message,
            )
            if (cleanupFailure !== failure) {
                failure.addSuppressed(cleanupFailure)
            }
        } finally {
            if (restoreInterrupt) {
                Thread.currentThread().interrupt()
            }
        }
    }

    private fun buildWarmupSandbox(): Sandbox {
        sandboxCreator?.let {
            return buildSandboxFromCreator(
                creator = it,
                idleTimeout = config.idleTimeout,
                reason = PooledSandboxCreateContext.Reason.WARMUP,
                readyTimeout = config.warmupReadyTimeout,
                healthCheckPollingInterval = config.warmupHealthCheckPollingInterval,
                skipHealthCheck = config.warmupSkipHealthCheck,
                customHealthCheck = config.warmupHealthCheck,
            )
        }

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

    private fun directCreate(
        sandboxTimeout: Duration?,
        policy: AcquirePolicy = AcquirePolicy.DIRECT_CREATE,
    ): Sandbox {
        // policy-aware namespace check: if the state store is down and the policy is a
        // fallthrough one, treat destroy-state as unknown and proceed to direct-create
        // instead of surfacing the outage. See ensurePoolNamespaceActiveForAcquire for
        // the full rationale.
        ensurePoolNamespaceActiveForAcquire(policy)
        sandboxCreator?.let {
            val sandbox =
                buildSandboxFromCreator(
                    creator = it,
                    idleTimeout = config.idleTimeout,
                    reason = PooledSandboxCreateContext.Reason.DIRECT_CREATE,
                    readyTimeout = config.acquireReadyTimeout,
                    healthCheckPollingInterval = config.acquireHealthCheckPollingInterval,
                    skipHealthCheck = config.acquireSkipHealthCheck,
                    customHealthCheck = config.acquireHealthCheck,
                )
            sandboxTimeout?.let { timeout ->
                try {
                    sandbox.renew(timeout)
                } catch (failure: Throwable) {
                    disposeSandboxAfterAcquireFailure(sandbox, failure)
                    throw failure
                }
            }
            ensurePoolNamespaceActiveOrDispose(sandbox, policy)
            return sandbox
        }

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
        // Renew is a candidate-specific step; failure means kill+close and rethrow.
        try {
            sandboxTimeout?.let { sandbox.renew(it) }
        } catch (failure: Throwable) {
            disposeSandboxAfterAcquireFailure(sandbox, failure)
            throw failure
        }
        // Post-create fence check uses the policy-aware helper so full state-store
        // outages degrade correctly under fallthrough policies; on rethrow the helper
        // has already killed and closed the sandbox.
        ensurePoolNamespaceActiveOrDispose(sandbox, policy)
        return sandbox
    }

    private fun ensurePoolNamespaceActive() {
        val state = stateStore.getDestroyState(config.poolName)
        if (state != PoolDestroyState.ACTIVE) {
            throw PoolDestroyedException("Pool namespace is $state: poolName=${config.poolName}")
        }
    }

    /**
     * Namespace-active check on the acquire path with graceful degradation.
     *
     * Same as [ensurePoolNamespaceActive], but when the state store itself is unavailable
     * ([PoolStateStoreUnavailableException]) and the effective [policy] falls through to
     * direct-create on empty idle, we treat the destroy state as *unknown* and allow the
     * acquire to proceed. This is the necessary counterpart to the state-store-outage
     * fallthrough already implemented at the `tryTakeIdle` and `directCreate` call sites
     * (see OSEP-0005 error-code matrix): without it a full Redis outage would short-circuit
     * acquire before the fallthrough branch could run, making `RETRY_NEXT_IDLE_THEN_CREATE`
     * and `DIRECT_CREATE` less available than documented.
     *
     * Fail-closed behavior for non-fallthrough policies ([AcquirePolicy.FAIL_FAST] /
     * [AcquirePolicy.RETRY_NEXT_IDLE]) is preserved: the outage is surfaced as-is so
     * callers can react.
     */
    private fun ensurePoolNamespaceActiveForAcquire(policy: AcquirePolicy) {
        try {
            ensurePoolNamespaceActive()
        } catch (e: PoolStateStoreUnavailableException) {
            if (!policyFallsThroughToDirectCreate(policy)) {
                throw e
            }
            logger.warn(
                "Acquire: state store unavailable during namespace check, " +
                    "assuming ACTIVE and degrading to direct-create per policy={} error={}",
                policy,
                e.message,
            )
        }
    }

    private fun throwIfPoolNamespaceDestroyed() {
        try {
            ensurePoolNamespaceActive()
        } catch (e: PoolDestroyedException) {
            throw e
        } catch (_: Exception) {
            return
        }
    }

    private fun isPoolNamespaceActive(): Boolean = stateStore.getDestroyState(config.poolName) == PoolDestroyState.ACTIVE

    /**
     * Post-create fence check.
     *
     * If [policy] is a fallthrough policy and the state store itself is unavailable
     * ([PoolStateStoreUnavailableException]), we cannot tell whether the pool was
     * destroyed, so we assume ACTIVE and keep the freshly-created sandbox (mirrors
     * the OSEP-0005 acquire-outage semantics). Non-fallthrough policies (and callers
     * that pass `policy=null`) keep the original fail-closed behavior for backward
     * compatibility.
     */
    private fun ensurePoolNamespaceActiveOrDispose(
        sandbox: Sandbox,
        policy: AcquirePolicy? = null,
    ) {
        try {
            ensurePoolNamespaceActive()
        } catch (e: PoolStateStoreUnavailableException) {
            if (policy != null && policyFallsThroughToDirectCreate(policy)) {
                logger.warn(
                    "Acquire: state store unavailable during post-create fence check, " +
                        "keeping sandbox and degrading per policy={} sandbox_id={} error={}",
                    policy,
                    sandbox.id,
                    e.message,
                )
                return
            }
            disposeSandboxAfterAcquireFailure(sandbox, e)
            throw e
        } catch (failure: Throwable) {
            disposeSandboxAfterAcquireFailure(sandbox, failure)
            throw failure
        }
    }

    private fun buildSandboxFromCreator(
        creator: PooledSandboxCreator,
        idleTimeout: Duration,
        reason: PooledSandboxCreateContext.Reason,
        readyTimeout: Duration,
        healthCheckPollingInterval: Duration,
        skipHealthCheck: Boolean,
        customHealthCheck: ((Sandbox) -> Boolean)?,
    ): Sandbox {
        val context =
            PooledSandboxCreateContext(
                poolName = config.poolName,
                ownerId = config.ownerId,
                idleTimeout = idleTimeout,
                reason = reason,
                readyTimeout = readyTimeout,
                healthCheckPollingInterval = healthCheckPollingInterval,
                skipHealthCheck = skipHealthCheck,
                healthCheck = customHealthCheck,
                connectionConfig = connectionConfig,
            )
        return creator.create(context)
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

    private fun beginOperation(run: RunContext) {
        run.inFlightOperations.incrementAndGet()
    }

    private fun endOperation(run: RunContext) {
        val remaining = run.inFlightOperations.decrementAndGet()
        if (remaining < 0) {
            run.inFlightOperations.set(0)
            logger.warn(
                "Pool in-flight counter underflow corrected: pool_name={} run={}",
                config.poolName,
                run.generation,
            )
            run.inFlightLock.lock()
            try {
                run.inFlightZero.signalAll()
            } finally {
                run.inFlightLock.unlock()
            }
            return
        }
        if (remaining == 0) {
            run.inFlightLock.lock()
            try {
                run.inFlightZero.signalAll()
            } finally {
                run.inFlightLock.unlock()
            }
        }
    }

    @Throws(InterruptedException::class)
    private fun awaitInFlightDrain(
        run: RunContext?,
        timeout: Duration,
    ): Boolean {
        if (run == null) return true
        val timeoutNanos = timeout.toNanos()
        if (timeoutNanos <= 0) {
            return run.inFlightOperations.get() == 0
        }
        val deadline = System.nanoTime() + timeoutNanos
        run.inFlightLock.lock()
        try {
            while (run.inFlightOperations.get() > 0) {
                val remaining = deadline - System.nanoTime()
                if (remaining <= 0) {
                    return false
                }
                run.inFlightZero.awaitNanos(remaining)
            }
            return true
        } finally {
            run.inFlightLock.unlock()
        }
    }

    private fun stopReconcile() {
        val run = currentRun
        run?.warmupSubmissionsOpen?.set(false)
        retireRun(run)
        reconcileTask?.cancel(true)
        reconcileTask = null
        primaryHeartbeatTask?.cancel(true)
        primaryHeartbeatTask = null
        warmupExecutor?.let { shutdownExecutor(it, "warmup") }
        warmupExecutor = null
        scheduler?.let { shutdownExecutor(it, "scheduler") }
        scheduler = null
        completePendingWarmupCompletions(run)
        run?.reconcileQueued?.set(false)
        releasePrimaryLockBestEffort(run)
    }

    private fun beginGracefulReconcileStop() {
        reconcileTask?.cancel(false)
        reconcileTask = null
    }

    private fun completeGracefulReconcileStop() {
        val run = currentRun
        retireRun(run)
        warmupExecutor?.let { shutdownExecutor(it, "warmup") }
        warmupExecutor = null
        primaryHeartbeatTask?.cancel(false)
        primaryHeartbeatTask = null
        scheduler?.let { shutdownExecutor(it, "scheduler") }
        scheduler = null
        completePendingWarmupCompletions(run)
        run?.reconcileQueued?.set(false)
        releasePrimaryLockBestEffort(run)
    }

    private fun forceStopReconcileAfterGracefulDrain() {
        val run = currentRun
        retireRun(run)
        // Stop warmup workers first while the controller remains alive so interrupted workers can
        // still deliver completion cleanup and release their tracked operation slots.
        warmupExecutor?.let { forceShutdownExecutor(it, "warmup") }
        warmupExecutor = null
        primaryHeartbeatTask?.cancel(true)
        primaryHeartbeatTask = null
        scheduler?.let { shutdownExecutor(it, "scheduler") }
        scheduler = null
        completePendingWarmupCompletions(run)
        run?.reconcileQueued?.set(false)
        releasePrimaryLockBestEffort(run)
    }

    private fun stopAfterNamespaceDestroyed() {
        if (!lifecycleState.compareAndSet(LifecycleState.RUNNING, LifecycleState.STOPPED)) return
        val run = currentRun
        run?.warmupSubmissionsOpen?.set(false)
        retireRun(run)
        reconcileTask?.cancel(false)
        reconcileTask = null
        primaryHeartbeatTask?.cancel(false)
        primaryHeartbeatTask = null
        warmupExecutor?.let { completeDroppedTasks(it.shutdownNow()) }
        warmupExecutor = null
        scheduler?.shutdown()
        scheduler = null
        completePendingWarmupCompletions(run)
        run?.reconcileQueued?.set(false)
        releasePrimaryLockBestEffort(run)
        closeProvider()
    }

    private fun releasePrimaryLockBestEffort(run: RunContext? = currentRun) {
        try {
            stateStore.releasePrimaryLock(config.poolName, config.ownerId)
        } catch (e: Exception) {
            logger.warn(
                "Pool primary lock release failed (best-effort): pool_name={} owner_id={} error={}",
                config.poolName,
                config.ownerId,
                e.message,
            )
        } finally {
            run?.primaryOwned?.set(false)
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
            completeDroppedTasks(dropped)
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
            completeDroppedTasks(dropped)
            Thread.currentThread().interrupt()
            logger.warn(
                "Pool {} executor shutdown interrupted; forced stop issued: pool_name={} dropped_tasks={}",
                role,
                config.poolName,
                dropped.size,
            )
        }
    }

    private fun forceShutdownExecutor(
        executor: ExecutorService,
        role: String,
    ) {
        val dropped = executor.shutdownNow()
        completeDroppedTasks(dropped)
        try {
            if (!executor.awaitTermination(5, TimeUnit.SECONDS)) {
                logger.warn(
                    "Pool {} executor did not terminate after forced stop: pool_name={} dropped_tasks={}",
                    role,
                    config.poolName,
                    dropped.size,
                )
            }
        } catch (_: InterruptedException) {
            Thread.currentThread().interrupt()
            logger.warn(
                "Pool {} executor forced stop wait interrupted: pool_name={} dropped_tasks={}",
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

    private fun isCurrentRun(run: RunContext): Boolean = currentRun === run && run.active.get()

    private fun retireRun(run: RunContext?) {
        if (run == null) return
        run.commitLock.lock()
        try {
            run.active.set(false)
            if (currentRun === run) {
                currentRun = null
            }
        } finally {
            run.commitLock.unlock()
        }
    }

    /**
     * Internal identity and executors for one start/shutdown lifecycle.
     *
     * Warmup workers capture this context so a task that outlives forced shutdown can only
     * complete against the scheduler that submitted it. [commitLock] linearizes retirement
     * with the final primary-lock renewal and idle-store commit.
     */
    private class RunContext(
        val generation: Long,
        val scheduler: ScheduledExecutorService,
        val warmupExecutor: ExecutorService,
    ) {
        val active = AtomicBoolean(true)
        val commitLock = ReentrantLock()
        val warmingCount = AtomicInteger(0)
        val warmupSubmissionsOpen = AtomicBoolean(true)
        val reconcileQueued = AtomicBoolean(false)

        @Volatile
        var nextCompletionReconcileAtNanos: Long = 0
        val primaryOwned = AtomicBoolean(false)
        val inFlightOperations = AtomicInteger(0)
        val inFlightLock = ReentrantLock()
        val inFlightZero: Condition = inFlightLock.newCondition()
        val pendingWarmupCompletions = ConcurrentHashMap.newKeySet<TrackedWarmupCompletionTask>()
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
        /** Minimum spacing between completion-driven reconcile ticks (see [requestReconcile]). */
        private const val COMPLETION_RECONCILE_MIN_INTERVAL_MS = 500L

        @JvmStatic
        fun builder(): Builder = Builder()

        private fun defaultIdleSandboxConnector(config: PoolConfig): (String) -> Sandbox =
            { sandboxId ->
                Sandbox.connector()
                    .sandboxId(sandboxId)
                    .connectTimeout(config.acquireReadyTimeout)
                    .healthCheckPollingInterval(config.acquireHealthCheckPollingInterval)
                    .skipHealthCheck(config.acquireSkipHealthCheck)
                    .connectionConfig(config.connectionConfig)
                    .run {
                        config.acquireHealthCheck?.let { healthCheck(it) } ?: this
                    }.connect()
            }
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

        fun sandboxCreator(sandboxCreator: PooledSandboxCreator): Builder {
            configBuilder.sandboxCreator(sandboxCreator)
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

        fun acquireMinRemainingTtl(acquireMinRemainingTtl: Duration): Builder {
            configBuilder.acquireMinRemainingTtl(acquireMinRemainingTtl)
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

        fun maxAcquireRetries(maxAcquireRetries: Int): Builder {
            configBuilder.maxAcquireRetries(maxAcquireRetries)
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
    credentialProxy?.let { configuredBuilder.credentialProxy(it) }
    platform?.let { configuredBuilder.platform(it) }
    return configuredBuilder
}
