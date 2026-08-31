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

import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import java.time.Duration
import java.util.UUID

/**
 * Configuration for a client-side sandbox pool.
 *
 * @property poolName User-defined name and namespace for this logical pool (required).
 * @property ownerId Unique process identity for primary lock ownership (node/process id, not pool id).
 * If not provided, a UUID-based default is generated.
 * @property maxIdle Standby idle target/cap (required).
 * @property warmupCreateQps Maximum warmup create admissions per one-second reconcile tick (default: 10).
 * @property warmupConcurrency Maximum concurrent post-create warmup workers (default: 128).
 * @property primaryLockTtl Lock TTL for distributed primary ownership (default: 60s).
 * @property stateStore Injected [PoolStateStore] implementation (required).
 * @property connectionConfig Connection config for lifecycle API (required).
 * @property creationSpec Template for creating sandboxes (replenish and direct-create) (required).
 * @property sandboxCreator Optional custom creator for pool-created sandboxes. When absent, the pool uses
 * [creationSpec] and the standard sandbox lifecycle API.
 * @property degradedThreshold Consecutive create failures required to transition to DEGRADED (default: 3).
 * @property acquireReadyTimeout Max time to wait for a sandbox returned by acquire to become ready (default: 30s).
 * @property acquireHealthCheckPollingInterval Poll interval while waiting for a sandbox returned by acquire to become
 * ready (default: 200ms).
 * @property acquireHealthCheck Optional custom health check for sandboxes returned by acquire.
 * @property acquireSkipHealthCheck When true, skip readiness checks for sandboxes returned by acquire (default: false).
 * @property acquireMinRemainingTtl Minimum remaining TTL an idle sandbox must have to be returned
 * by acquire. Idle entries closer to expiry than this threshold are discarded so the subsequent
 * ready-check and any user-side renew have time to run before server-side expiry. Set to
 * [Duration.ZERO] to opt out and restore the pre-existing binary-expiry behavior.
 *
 * Default is auto-derived from [idleTimeout] so existing users with short idle timeouts are not
 * silently broken: 60s when [idleTimeout] > 60s, otherwise `idleTimeout / 2` (rounded down). The
 * resolved value is always strictly less than [idleTimeout]. Pass an explicit value to the builder
 * to override.
 * @property warmupReadyTimeout Max time to wait for a pool-created sandbox to become ready (default: 30s).
 * @property warmupHealthCheckInitialDelay Delay before the first warmup readiness check (default: zero).
 * @property warmupHealthCheckPollingInterval Poll interval while waiting for a pool-created sandbox to become ready
 * (default: 500ms).
 * @property warmupHealthCheck Optional custom health check for pool-created sandboxes.
 * @property warmupSandboxPreparer Optional callback invoked after a warmup sandbox is ready and before it is put idle.
 * @property warmupPostPrepareHealthCheck Optional health check invoked after the warmup preparer succeeds.
 * @property warmupPostPrepareHealthCheckTimeout Max time to wait for the post-prepare health check (default: 30s).
 * @property warmupSkipHealthCheck When true, skip readiness checks for pool-created sandboxes (default: false).
 * @property idleTimeout Timeout applied to pool-created sandboxes when they are initialized (default: 24h).
 * @property drainTimeout Max wait during graceful shutdown for in-flight ops (default: 30s).
 * @property maxAcquireRetries Maximum number of idle candidates that a single acquire may attempt
 * when the effective policy is [AcquirePolicy.RETRY_NEXT_IDLE] or
 * [AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE]. Counts the total attempts, not additional retries:
 * `1` disables retry (matches [AcquirePolicy.FAIL_FAST] / [AcquirePolicy.DIRECT_CREATE] behavior),
 * `3` (default) tries up to three idles before giving up or falling through. Ignored under
 * [AcquirePolicy.FAIL_FAST] / [AcquirePolicy.DIRECT_CREATE], which always try at most one idle.
 * Must be >= 1. Increasing this trades acquire latency (each failed candidate pays up to
 * `acquireReadyTimeout`) for a higher chance of returning a warm sandbox.
 */
class PoolConfig private constructor(
    val poolName: String,
    val ownerId: String,
    val maxIdle: Int,
    val warmupCreateQps: Int,
    val warmupConcurrency: Int,
    val primaryLockTtl: java.time.Duration,
    val stateStore: PoolStateStore,
    val connectionConfig: ConnectionConfig,
    val creationSpec: PoolCreationSpec,
    val sandboxCreator: PooledSandboxCreator?,
    val degradedThreshold: Int,
    val acquireReadyTimeout: Duration,
    val acquireHealthCheckPollingInterval: Duration,
    val acquireHealthCheck: ((Sandbox) -> Boolean)?,
    val acquireSkipHealthCheck: Boolean,
    val acquireMinRemainingTtl: Duration,
    val warmupReadyTimeout: Duration,
    val warmupHealthCheckInitialDelay: Duration,
    val warmupHealthCheckPollingInterval: Duration,
    val warmupHealthCheck: ((Sandbox) -> Boolean)?,
    val warmupSandboxPreparer: SandboxPreparer?,
    val warmupPostPrepareHealthCheck: ((Sandbox) -> Boolean)?,
    val warmupPostPrepareHealthCheckTimeout: Duration,
    val warmupSkipHealthCheck: Boolean,
    val idleTimeout: Duration,
    val drainTimeout: Duration,
    val maxAcquireRetries: Int,
) {
    init {
        require(poolName.isNotBlank()) { "poolName must not be blank" }
        require(ownerId.isNotBlank()) { "ownerId must not be blank" }
        require(maxIdle >= 0) { "maxIdle must be >= 0" }
        require(warmupCreateQps > 0) { "warmupCreateQps must be positive" }
        require(warmupConcurrency > 0) { "warmupConcurrency must be positive" }
        require(degradedThreshold > 0) { "degradedThreshold must be positive" }
        require(!primaryLockTtl.isNegative && !primaryLockTtl.isZero) { "primaryLockTtl must be positive" }
        require(!acquireReadyTimeout.isNegative && !acquireReadyTimeout.isZero) {
            "acquireReadyTimeout must be positive"
        }
        require(!acquireHealthCheckPollingInterval.isNegative && !acquireHealthCheckPollingInterval.isZero) {
            "acquireHealthCheckPollingInterval must be positive"
        }
        require(!acquireMinRemainingTtl.isNegative) { "acquireMinRemainingTtl must be non-negative" }
        require(acquireMinRemainingTtl < idleTimeout) {
            "acquireMinRemainingTtl ($acquireMinRemainingTtl) must be strictly less than " +
                "idleTimeout ($idleTimeout); otherwise every warmed idle entry would be rejected"
        }
        require(!warmupReadyTimeout.isNegative && !warmupReadyTimeout.isZero) { "warmupReadyTimeout must be positive" }
        require(!warmupHealthCheckInitialDelay.isNegative) { "warmupHealthCheckInitialDelay must be non-negative" }
        require(!warmupHealthCheckPollingInterval.isNegative && !warmupHealthCheckPollingInterval.isZero) {
            "warmupHealthCheckPollingInterval must be positive"
        }
        require(!warmupPostPrepareHealthCheckTimeout.isNegative && !warmupPostPrepareHealthCheckTimeout.isZero) {
            "warmupPostPrepareHealthCheckTimeout must be positive"
        }
        require(!idleTimeout.isNegative && !idleTimeout.isZero) { "idleTimeout must be positive" }
        require(!drainTimeout.isNegative) { "drainTimeout must be non-negative" }
        require(maxAcquireRetries >= 1) { "maxAcquireRetries must be >= 1" }
    }

    companion object {
        private val DEFAULT_PRIMARY_LOCK_TTL = Duration.ofSeconds(60)
        private const val DEFAULT_DEGRADED_THRESHOLD = 3
        private const val DEFAULT_WARMUP_CREATE_QPS = 10
        private const val DEFAULT_WARMUP_CONCURRENCY = 128
        private val DEFAULT_ACQUIRE_READY_TIMEOUT = Duration.ofSeconds(30)
        private val DEFAULT_ACQUIRE_HEALTH_CHECK_POLLING_INTERVAL = Duration.ofMillis(200)
        private val DEFAULT_ACQUIRE_MIN_REMAINING_TTL_CAP: Duration = Duration.ofSeconds(60)
        private val DEFAULT_WARMUP_READY_TIMEOUT = Duration.ofSeconds(30)
        private val DEFAULT_WARMUP_HEALTH_CHECK_INITIAL_DELAY = Duration.ZERO
        private val DEFAULT_WARMUP_HEALTH_CHECK_POLLING_INTERVAL = Duration.ofMillis(500)
        private val DEFAULT_WARMUP_POST_PREPARE_HEALTH_CHECK_TIMEOUT = Duration.ofSeconds(30)
        private val DEFAULT_IDLE_TIMEOUT = Duration.ofHours(24)
        private val DEFAULT_DRAIN_TIMEOUT = Duration.ofSeconds(30)
        private const val DEFAULT_MAX_ACQUIRE_RETRIES = 3

        @JvmStatic
        fun builder(): Builder = Builder()

        /**
         * Resolves the default `acquireMinRemainingTtl` from the user's [idleTimeout]:
         * `min(60s, idleTimeout / 2)`. The result is always strictly less than [idleTimeout],
         * so users with short idle timeouts get an automatically scaled threshold instead of a
         * config-time error.
         */
        internal fun defaultAcquireMinRemainingTtl(idleTimeout: Duration): Duration {
            val half = idleTimeout.dividedBy(2L)
            return if (DEFAULT_ACQUIRE_MIN_REMAINING_TTL_CAP < half) {
                DEFAULT_ACQUIRE_MIN_REMAINING_TTL_CAP
            } else {
                half
            }
        }
    }

    internal fun withMaxIdle(maxIdle: Int): PoolConfig {
        return PoolConfig(
            poolName = poolName,
            ownerId = ownerId,
            maxIdle = maxIdle,
            warmupCreateQps = warmupCreateQps,
            warmupConcurrency = warmupConcurrency,
            primaryLockTtl = primaryLockTtl,
            stateStore = stateStore,
            connectionConfig = connectionConfig,
            creationSpec = creationSpec,
            sandboxCreator = sandboxCreator,
            degradedThreshold = degradedThreshold,
            acquireReadyTimeout = acquireReadyTimeout,
            acquireHealthCheckPollingInterval = acquireHealthCheckPollingInterval,
            acquireHealthCheck = acquireHealthCheck,
            acquireSkipHealthCheck = acquireSkipHealthCheck,
            acquireMinRemainingTtl = acquireMinRemainingTtl,
            warmupReadyTimeout = warmupReadyTimeout,
            warmupHealthCheckInitialDelay = warmupHealthCheckInitialDelay,
            warmupHealthCheckPollingInterval = warmupHealthCheckPollingInterval,
            warmupHealthCheck = warmupHealthCheck,
            warmupSandboxPreparer = warmupSandboxPreparer,
            warmupPostPrepareHealthCheck = warmupPostPrepareHealthCheck,
            warmupPostPrepareHealthCheckTimeout = warmupPostPrepareHealthCheckTimeout,
            warmupSkipHealthCheck = warmupSkipHealthCheck,
            idleTimeout = idleTimeout,
            drainTimeout = drainTimeout,
            maxAcquireRetries = maxAcquireRetries,
        )
    }

    class Builder {
        private var poolName: String? = null
        private var ownerId: String? = null
        private var maxIdle: Int? = null
        private var warmupCreateQps: Int = DEFAULT_WARMUP_CREATE_QPS
        private var warmupConcurrency: Int = DEFAULT_WARMUP_CONCURRENCY
        private var primaryLockTtl: Duration = DEFAULT_PRIMARY_LOCK_TTL
        private var stateStore: PoolStateStore? = null
        private var connectionConfig: ConnectionConfig? = null
        private var creationSpec: PoolCreationSpec? = null
        private var sandboxCreator: PooledSandboxCreator? = null
        private var degradedThreshold: Int = DEFAULT_DEGRADED_THRESHOLD
        private var acquireReadyTimeout: Duration = DEFAULT_ACQUIRE_READY_TIMEOUT
        private var acquireHealthCheckPollingInterval: Duration = DEFAULT_ACQUIRE_HEALTH_CHECK_POLLING_INTERVAL
        private var acquireHealthCheck: ((Sandbox) -> Boolean)? = null
        private var acquireSkipHealthCheck: Boolean = false
        private var acquireMinRemainingTtl: Duration? = null
        private var warmupReadyTimeout: Duration = DEFAULT_WARMUP_READY_TIMEOUT
        private var warmupHealthCheckInitialDelay: Duration = DEFAULT_WARMUP_HEALTH_CHECK_INITIAL_DELAY
        private var warmupHealthCheckPollingInterval: Duration = DEFAULT_WARMUP_HEALTH_CHECK_POLLING_INTERVAL
        private var warmupHealthCheck: ((Sandbox) -> Boolean)? = null
        private var warmupSandboxPreparer: SandboxPreparer? = null
        private var warmupPostPrepareHealthCheck: ((Sandbox) -> Boolean)? = null
        private var warmupPostPrepareHealthCheckTimeout: Duration =
            DEFAULT_WARMUP_POST_PREPARE_HEALTH_CHECK_TIMEOUT
        private var warmupSkipHealthCheck: Boolean = false
        private var idleTimeout: Duration = DEFAULT_IDLE_TIMEOUT
        private var drainTimeout: Duration = DEFAULT_DRAIN_TIMEOUT
        private var maxAcquireRetries: Int = DEFAULT_MAX_ACQUIRE_RETRIES

        fun poolName(poolName: String): Builder {
            this.poolName = poolName
            return this
        }

        fun ownerId(ownerId: String): Builder {
            this.ownerId = ownerId
            return this
        }

        fun maxIdle(maxIdle: Int): Builder {
            this.maxIdle = maxIdle
            return this
        }

        fun warmupCreateQps(warmupCreateQps: Int): Builder {
            this.warmupCreateQps = warmupCreateQps
            return this
        }

        fun warmupConcurrency(warmupConcurrency: Int): Builder {
            this.warmupConcurrency = warmupConcurrency
            return this
        }

        fun primaryLockTtl(primaryLockTtl: Duration): Builder {
            this.primaryLockTtl = primaryLockTtl
            return this
        }

        fun stateStore(stateStore: PoolStateStore): Builder {
            this.stateStore = stateStore
            return this
        }

        fun connectionConfig(connectionConfig: ConnectionConfig): Builder {
            this.connectionConfig = connectionConfig
            return this
        }

        fun creationSpec(creationSpec: PoolCreationSpec): Builder {
            this.creationSpec = creationSpec
            return this
        }

        fun sandboxCreator(sandboxCreator: PooledSandboxCreator): Builder {
            this.sandboxCreator = sandboxCreator
            return this
        }

        fun degradedThreshold(degradedThreshold: Int): Builder {
            this.degradedThreshold = degradedThreshold
            return this
        }

        fun acquireReadyTimeout(acquireReadyTimeout: Duration): Builder {
            this.acquireReadyTimeout = acquireReadyTimeout
            return this
        }

        fun acquireHealthCheckPollingInterval(acquireHealthCheckPollingInterval: Duration): Builder {
            this.acquireHealthCheckPollingInterval = acquireHealthCheckPollingInterval
            return this
        }

        fun acquireHealthCheck(acquireHealthCheck: (Sandbox) -> Boolean): Builder {
            this.acquireHealthCheck = acquireHealthCheck
            return this
        }

        fun acquireSkipHealthCheck(acquireSkipHealthCheck: Boolean = true): Builder {
            this.acquireSkipHealthCheck = acquireSkipHealthCheck
            return this
        }

        /**
         * Sets the minimum remaining TTL an idle sandbox must have to be returned by acquire.
         * Idle entries closer to expiry than [acquireMinRemainingTtl] are discarded so the
         * subsequent ready-check and any user-side renew have time to run before the server-side
         * expiry kicks in.
         *
         * Must be non-negative and strictly less than `idleTimeout`. If not set, the resolved
         * default is `min(60s, idleTimeout / 2)`. Pass [Duration.ZERO] to opt out and restore the
         * pre-existing binary-expiry behavior.
         */
        fun acquireMinRemainingTtl(acquireMinRemainingTtl: Duration): Builder {
            this.acquireMinRemainingTtl = acquireMinRemainingTtl
            return this
        }

        fun warmupReadyTimeout(warmupReadyTimeout: Duration): Builder {
            this.warmupReadyTimeout = warmupReadyTimeout
            return this
        }

        fun warmupHealthCheckInitialDelay(warmupHealthCheckInitialDelay: Duration): Builder {
            this.warmupHealthCheckInitialDelay = warmupHealthCheckInitialDelay
            return this
        }

        fun warmupHealthCheckPollingInterval(warmupHealthCheckPollingInterval: Duration): Builder {
            this.warmupHealthCheckPollingInterval = warmupHealthCheckPollingInterval
            return this
        }

        fun warmupHealthCheck(warmupHealthCheck: (Sandbox) -> Boolean): Builder {
            this.warmupHealthCheck = warmupHealthCheck
            return this
        }

        fun warmupSandboxPreparer(warmupSandboxPreparer: SandboxPreparer): Builder {
            this.warmupSandboxPreparer = warmupSandboxPreparer
            return this
        }

        fun warmupPostPrepareHealthCheck(warmupPostPrepareHealthCheck: (Sandbox) -> Boolean): Builder {
            this.warmupPostPrepareHealthCheck = warmupPostPrepareHealthCheck
            return this
        }

        fun warmupPostPrepareHealthCheckTimeout(warmupPostPrepareHealthCheckTimeout: Duration): Builder {
            this.warmupPostPrepareHealthCheckTimeout = warmupPostPrepareHealthCheckTimeout
            return this
        }

        fun warmupSkipHealthCheck(warmupSkipHealthCheck: Boolean = true): Builder {
            this.warmupSkipHealthCheck = warmupSkipHealthCheck
            return this
        }

        fun idleTimeout(idleTimeout: Duration): Builder {
            this.idleTimeout = idleTimeout
            return this
        }

        fun drainTimeout(drainTimeout: Duration): Builder {
            this.drainTimeout = drainTimeout
            return this
        }

        /**
         * Sets the upper bound on how many idle candidates a single acquire will attempt when the
         * effective policy is [AcquirePolicy.RETRY_NEXT_IDLE] or
         * [AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE]. Must be >= 1 (1 disables retry).
         * Default: 3.
         */
        fun maxAcquireRetries(maxAcquireRetries: Int): Builder {
            this.maxAcquireRetries = maxAcquireRetries
            return this
        }

        private fun generateDefaultOwnerId(): String {
            return "pool-owner-${UUID.randomUUID()}"
        }

        fun build(): PoolConfig {
            val name = poolName ?: throw IllegalArgumentException("poolName is required")
            val owner = ownerId ?: generateDefaultOwnerId()
            val max = maxIdle ?: throw IllegalArgumentException("maxIdle is required")
            val store = stateStore ?: throw IllegalArgumentException("stateStore is required")
            val conn = connectionConfig ?: throw IllegalArgumentException("connectionConfig is required")
            val spec = creationSpec ?: throw IllegalArgumentException("creationSpec is required")

            val resolvedAcquireMinRemainingTtl =
                acquireMinRemainingTtl ?: defaultAcquireMinRemainingTtl(idleTimeout)

            return PoolConfig(
                poolName = name,
                ownerId = owner,
                maxIdle = max,
                warmupCreateQps = warmupCreateQps,
                warmupConcurrency = warmupConcurrency,
                primaryLockTtl = primaryLockTtl,
                stateStore = store,
                connectionConfig = conn,
                creationSpec = spec,
                sandboxCreator = sandboxCreator,
                degradedThreshold = degradedThreshold,
                acquireReadyTimeout = acquireReadyTimeout,
                acquireHealthCheckPollingInterval = acquireHealthCheckPollingInterval,
                acquireHealthCheck = acquireHealthCheck,
                acquireSkipHealthCheck = acquireSkipHealthCheck,
                acquireMinRemainingTtl = resolvedAcquireMinRemainingTtl,
                warmupReadyTimeout = warmupReadyTimeout,
                warmupHealthCheckInitialDelay = warmupHealthCheckInitialDelay,
                warmupHealthCheckPollingInterval = warmupHealthCheckPollingInterval,
                warmupHealthCheck = warmupHealthCheck,
                warmupSandboxPreparer = warmupSandboxPreparer,
                warmupPostPrepareHealthCheck = warmupPostPrepareHealthCheck,
                warmupPostPrepareHealthCheckTimeout = warmupPostPrepareHealthCheckTimeout,
                warmupSkipHealthCheck = warmupSkipHealthCheck,
                idleTimeout = idleTimeout,
                drainTimeout = drainTimeout,
                maxAcquireRetries = maxAcquireRetries,
            )
        }
    }
}
