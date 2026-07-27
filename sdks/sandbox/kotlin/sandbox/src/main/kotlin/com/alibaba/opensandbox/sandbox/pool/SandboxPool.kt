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
    private val sandboxCreator: PooledSandboxCreator? = config.sandboxCreator
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
            ensurePoolNamespaceActive()
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
        beginOperation()
        try {
            if (lifecycleState.get() != LifecycleState.RUNNING) {
                val state = lifecycleState.get()
                throwIfPoolNamespaceDestroyed()
                logger.info("Pool not running after acquire started, rejected: pool_name={} state={}", config.poolName, state)
                throw PoolNotRunningException("Cannot acquire when pool state is $state")
            }
            ensurePoolNamespaceActiveForAcquire(policy)
            val poolName = config.poolName
            val maxAttempts = effectiveMaxIdleAttempts(policy)
            // Accumulate discarded-alive sandbox IDs across all take iterations so we schedule
            // a single deferred cleanup, instead of one kill batch per retry.
            val pendingKill = ArrayList<String>()
            var lastSandboxId: String? = null
            var lastIdleConnectFailure: Exception? = null
            var attemptedAny = false
            var attempt = 0
            var loopExhausted = true
            while (attempt < maxAttempts) {
                attempt++
                val takeResult =
                    try {
                        stateStore.tryTakeIdle(poolName, config.acquireMinRemainingTtl)
                    } catch (e: PoolStateStoreUnavailableException) {
                        // State store outage. Per OSEP-0005, under policies that fall through to
                        // direct-create on empty idle we degrade to that fallback so the pool
                        // stays at least as available as raw SDK usage during store outages.
                        // Under non-fallthrough policies (FAIL_FAST / RETRY_NEXT_IDLE) we surface
                        // the outage as-is so callers can react.
                        if (!policyFallsThroughToDirectCreate(policy)) {
                            scheduleKillDiscardedAlive(poolName, pendingKill, source = "acquire")
                            throw e
                        }
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
                    sandbox =
                        Sandbox.connector()
                            .sandboxId(sandboxId)
                            .connectTimeout(config.acquireReadyTimeout)
                            .healthCheckPollingInterval(config.acquireHealthCheckPollingInterval)
                            .skipHealthCheck(config.acquireSkipHealthCheck)
                            .connectionConfig(connectionConfig)
                            .run {
                                config.acquireHealthCheck?.let { healthCheck(it) } ?: this
                            }.connect()
                } catch (e: PoolDestroyedException) {
                    scheduleKillDiscardedAlive(poolName, pendingKill, source = "acquire")
                    throw e
                } catch (e: Exception) {
                    // Connect / readiness / health-check failure — the idle candidate itself is
                    // unusable. Remove it, best-effort kill, then let the loop try the next.
                    lastIdleConnectFailure = e
                    logger.warn(
                        "Idle connect failed (stale or unreachable), removed from pool: " +
                            "pool_name={} sandbox_id={} policy={} attempt={}/{} error={}",
                        poolName,
                        sandboxId,
                        policy,
                        attempt,
                        maxAttempts,
                        e.message,
                    )
                    stateStore.removeIdle(poolName, sandboxId)
                    // Fire-and-forget the remote kill on the warmup executor. Awaiting kill
                    // inline would let a slow DELETE (up to the lifecycle client's request
                    // timeout) block the next retry iteration, defeating the point of
                    // RETRY_NEXT_IDLE. scheduleKillDiscardedAlive uses the same executor path
                    // as discarded-alive cleanup and falls back to inline execution when the
                    // pool is mid-shutdown so cleanup is never silently dropped.
                    scheduleKillDiscardedAlive(poolName, listOf(sandboxId), source = "acquire-stale")
                    // Re-check lifecycle between iterations so an in-flight shutdown / namespace
                    // destroy short-circuits the retry loop instead of paying another readyTimeout.
                    if (lifecycleState.get() != LifecycleState.RUNNING) {
                        val state = lifecycleState.get()
                        throwIfPoolNamespaceDestroyed()
                        scheduleKillDiscardedAlive(poolName, pendingKill, source = "acquire")
                        throw PoolNotRunningException("Cannot acquire when pool state is $state")
                    }
                    ensurePoolNamespaceActive()
                    continue
                }
                // Connect + readiness succeeded. From here on the sandbox is a healthy, borrowable
                // idle: any failure below (renew rejection, namespace fenced) is NOT a candidate-
                // specific problem, so we must not treat it as "stale idle" and burn another retry.
                // Dispose the sandbox and rethrow instead.
                try {
                    sandboxTimeout?.let { sandbox.renew(it) }
                    ensurePoolNamespaceActiveOrDispose(sandbox)
                } catch (e: PoolDestroyedException) {
                    scheduleKillDiscardedAlive(poolName, pendingKill, source = "acquire")
                    throw e
                } catch (e: Exception) {
                    // Renew failed against a healthy sandbox. Retrying another idle will not fix a
                    // lifecycle-API renew rejection, so we must not loop. But `tryTakeIdle` has
                    // already popped this ID out of the idle store — if we only close() locally,
                    // the remote sandbox stays alive on the server until its TTL expires and is
                    // no longer tracked anywhere (leaks against pool capacity accounting). Kill
                    // the remote sandbox best-effort, then close local resources and rethrow.
                    logger.warn(
                        "Acquire renew failed after idle connect; killing remote sandbox and not " +
                            "retrying (renew errors are not candidate-specific): " +
                            "pool_name={} sandbox_id={} policy={} error={}",
                        poolName,
                        sandboxId,
                        policy,
                        e.message,
                    )
                    try {
                        sandbox.kill()
                    } catch (killEx: Exception) {
                        logger.warn(
                            "Best-effort kill after renew failure failed: pool_name={} sandbox_id={} error={}",
                            poolName,
                            sandboxId,
                            killEx.message,
                        )
                    }
                    try {
                        sandbox.close()
                    } catch (_: Exception) {
                        // best-effort local resource release
                    }
                    scheduleKillDiscardedAlive(poolName, pendingKill, source = "acquire")
                    throw e
                }
                // Candidate is connected and renewed. Now safe to clean up the discarded-alive
                // sandboxes; offload to the warmup executor so the caller does not wait for
                // N kill RPCs.
                scheduleKillDiscardedAlive(poolName, pendingKill, source = "acquire")
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
            // Reaching here means we did not return a sandbox from idle. Fire the deferred cleanup
            // so the discarded-alive sandboxes do not linger; both the fail-fast and direct-create
            // fallthroughs below benefit from asynchronous cleanup instead of a synchronous kill.
            scheduleKillDiscardedAlive(poolName, pendingKill, source = "acquire")

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
            ensurePoolNamespaceActiveForAcquire(policy)
            logger.debug("Acquire direct create: pool_name={} reason={} policy={}", poolName, reason, policy)
            return directCreate(sandboxTimeout, policy)
        } finally {
            endOperation()
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

    /**
     * Offload [killDiscardedAlive] to the warmup executor so the caller does not block on the
     * kill RPCs. Falls back to inline execution when no executor is available (e.g. the pool is
     * shutting down) — better to slow the caller than to drop the cleanup entirely.
     */
    private fun scheduleKillDiscardedAlive(
        poolName: String,
        sandboxIds: List<String>,
        source: String,
    ) {
        if (sandboxIds.isEmpty()) return
        val executor = warmupExecutor
        if (executor == null) {
            killDiscardedAlive(poolName, sandboxIds, source)
            return
        }
        try {
            executor.submit {
                killDiscardedAlive(poolName, sandboxIds, source)
            }
        } catch (e: Exception) {
            // Executor may reject if the pool is mid-shutdown; fall back to inline kill.
            logger.debug(
                "Discarded-alive kill submit rejected, running inline: pool_name={} count={} error={}",
                poolName,
                sandboxIds.size,
                e.message,
            )
            killDiscardedAlive(poolName, sandboxIds, source)
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
        val manager = sandboxManager ?: return
        for (sandboxId in sandboxIds) {
            try {
                manager.killSandbox(sandboxId)
                logger.debug(
                    "Killed near-expiry idle sandbox: pool_name={} sandbox_id={} source={}",
                    poolName,
                    sandboxId,
                    source,
                )
            } catch (e: Exception) {
                logger.warn(
                    "Failed to kill near-expiry idle sandbox (best-effort, will expire server-side): " +
                        "pool_name={} sandbox_id={} source={} error={}",
                    poolName,
                    sandboxId,
                    source,
                    e.message,
                )
            }
        }
    }

    private fun createSandboxManager(): SandboxManager = sandboxManagerFactory(connectionConfig.copyWithoutConnectionPool())

    private fun runReconcileTick() {
        if (lifecycleState.get() != LifecycleState.RUNNING) return
        if (!isPoolNamespaceActive()) {
            logger.info("Pool namespace is destroyed; stopping local pool: pool_name={}", config.poolName)
            stopAfterNamespaceDestroyed()
            return
        }
        val executor = warmupExecutor ?: return
        beginOperation()
        try {
            if (lifecycleState.get() != LifecycleState.RUNNING) return
            if (!isPoolNamespaceActive()) return
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
     * Creates one sandbox, waits for readiness, then returns its id. Caller must put the
     * id into the store; the created [Sandbox] is closed immediately so only the id is
     * kept in the pool.
     */
    private fun createOneSandbox(): String? {
        beginOperation()
        return try {
            val sandbox = buildWarmupSandbox()
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
                } catch (e: Exception) {
                    try {
                        sandbox.kill()
                    } finally {
                        sandbox.close()
                    }
                    throw e
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
        } catch (e: Exception) {
            try {
                sandbox.kill()
            } finally {
                sandbox.close()
            }
            throw e
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
            try {
                sandbox.kill()
            } catch (cleanupError: Exception) {
                logger.warn(
                    "Pool sandbox cleanup after store-outage fence failed: pool_name={} sandbox_id={} operation=kill error={}",
                    config.poolName,
                    sandbox.id,
                    cleanupError.message,
                )
            }
            try {
                sandbox.close()
            } catch (cleanupError: Exception) {
                logger.warn(
                    "Pool sandbox cleanup after store-outage fence failed: pool_name={} sandbox_id={} operation=close error={}",
                    config.poolName,
                    sandbox.id,
                    cleanupError.message,
                )
            }
            throw e
        } catch (e: Exception) {
            try {
                sandbox.kill()
            } catch (cleanupError: Exception) {
                logger.warn(
                    "Pool sandbox cleanup after fence failed: pool_name={} sandbox_id={} operation=kill error={}",
                    config.poolName,
                    sandbox.id,
                    cleanupError.message,
                )
            }
            try {
                sandbox.close()
            } catch (cleanupError: Exception) {
                logger.warn(
                    "Pool sandbox cleanup after fence failed: pool_name={} sandbox_id={} operation=close error={}",
                    config.poolName,
                    sandbox.id,
                    cleanupError.message,
                )
            }
            throw e
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

    private fun stopAfterNamespaceDestroyed() {
        if (!lifecycleState.compareAndSet(LifecycleState.RUNNING, LifecycleState.STOPPED)) return
        reconcileTask?.cancel(false)
        reconcileTask = null
        scheduler?.shutdown()
        scheduler = null
        warmupExecutor?.shutdownNow()
        warmupExecutor = null
        releasePrimaryLockBestEffort()
        closeProvider()
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
