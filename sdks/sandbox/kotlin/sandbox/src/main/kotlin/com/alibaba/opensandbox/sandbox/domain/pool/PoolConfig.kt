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
import kotlin.math.ceil

/**
 * Configuration for a client-side sandbox pool.
 *
 * @property poolName User-defined name and namespace for this logical pool (required).
 * @property ownerId Unique process identity for primary lock ownership (node/process id, not pool id).
 * If not provided, a UUID-based default is generated.
 * @property maxIdle Standby idle target/cap (required).
 * @property warmupConcurrency Max concurrent creation workers during replenish (default: max(1, ceil(maxIdle * 0.2))).
 * @property primaryLockTtl Lock TTL for distributed primary ownership (default: 60s).
 * @property stateStore Injected [PoolStateStore] implementation (required).
 * @property connectionConfig Connection config for lifecycle API (required).
 * @property creationSpec Template for creating sandboxes (replenish and direct-create) (required).
 * @property reconcileInterval Interval between reconcile ticks (default: 30s).
 * @property degradedThreshold Consecutive create failures required to transition to DEGRADED (default: 3).
 * @property acquireReadyTimeout Max time to wait for a sandbox returned by acquire to become ready (default: 30s).
 * @property acquireHealthCheckPollingInterval Poll interval while waiting for a sandbox returned by acquire to become
 * ready (default: 200ms).
 * @property acquireHealthCheck Optional custom health check for sandboxes returned by acquire.
 * @property acquireSkipHealthCheck When true, skip readiness checks for sandboxes returned by acquire (default: false).
 * @property warmupReadyTimeout Max time to wait for a pool-created sandbox to become ready (default: 30s).
 * @property warmupHealthCheckPollingInterval Poll interval while waiting for a pool-created sandbox to become ready
 * (default: 200ms).
 * @property warmupHealthCheck Optional custom health check for pool-created sandboxes.
 * @property warmupSandboxPreparer Optional callback invoked after a warmup sandbox is ready and before it is put idle.
 * @property warmupSkipHealthCheck When true, skip readiness checks for pool-created sandboxes (default: false).
 * @property idleTimeout Timeout applied to pool-created sandboxes when they are initialized (default: 24h).
 * @property drainTimeout Max wait during graceful shutdown for in-flight ops (default: 30s).
 */
class PoolConfig private constructor(
    val poolName: String,
    val ownerId: String,
    val maxIdle: Int,
    val warmupConcurrency: Int,
    val primaryLockTtl: java.time.Duration,
    val stateStore: PoolStateStore,
    val connectionConfig: ConnectionConfig,
    val creationSpec: PoolCreationSpec,
    val reconcileInterval: Duration,
    val degradedThreshold: Int,
    val acquireReadyTimeout: Duration,
    val acquireHealthCheckPollingInterval: Duration,
    val acquireHealthCheck: ((Sandbox) -> Boolean)?,
    val acquireSkipHealthCheck: Boolean,
    val warmupReadyTimeout: Duration,
    val warmupHealthCheckPollingInterval: Duration,
    val warmupHealthCheck: ((Sandbox) -> Boolean)?,
    val warmupSandboxPreparer: SandboxPreparer?,
    val warmupSkipHealthCheck: Boolean,
    val idleTimeout: Duration,
    val drainTimeout: Duration,
) {
    init {
        require(poolName.isNotBlank()) { "poolName must not be blank" }
        require(ownerId.isNotBlank()) { "ownerId must not be blank" }
        require(maxIdle >= 0) { "maxIdle must be >= 0" }
        require(warmupConcurrency > 0) { "warmupConcurrency must be positive" }
        require(degradedThreshold > 0) { "degradedThreshold must be positive" }
        require(!reconcileInterval.isNegative && !reconcileInterval.isZero) { "reconcileInterval must be positive" }
        require(!primaryLockTtl.isNegative && !primaryLockTtl.isZero) { "primaryLockTtl must be positive" }
        require(!acquireReadyTimeout.isNegative && !acquireReadyTimeout.isZero) {
            "acquireReadyTimeout must be positive"
        }
        require(!acquireHealthCheckPollingInterval.isNegative && !acquireHealthCheckPollingInterval.isZero) {
            "acquireHealthCheckPollingInterval must be positive"
        }
        require(!warmupReadyTimeout.isNegative && !warmupReadyTimeout.isZero) { "warmupReadyTimeout must be positive" }
        require(!warmupHealthCheckPollingInterval.isNegative && !warmupHealthCheckPollingInterval.isZero) {
            "warmupHealthCheckPollingInterval must be positive"
        }
        require(!idleTimeout.isNegative && !idleTimeout.isZero) { "idleTimeout must be positive" }
        require(!drainTimeout.isNegative) { "drainTimeout must be non-negative" }
    }

    companion object {
        private val DEFAULT_RECONCILE_INTERVAL = Duration.ofSeconds(30)
        private val DEFAULT_PRIMARY_LOCK_TTL = Duration.ofSeconds(60)
        private const val DEFAULT_DEGRADED_THRESHOLD = 3
        private val DEFAULT_ACQUIRE_READY_TIMEOUT = Duration.ofSeconds(30)
        private val DEFAULT_ACQUIRE_HEALTH_CHECK_POLLING_INTERVAL = Duration.ofMillis(200)
        private val DEFAULT_WARMUP_READY_TIMEOUT = Duration.ofSeconds(30)
        private val DEFAULT_WARMUP_HEALTH_CHECK_POLLING_INTERVAL = Duration.ofMillis(200)
        private val DEFAULT_IDLE_TIMEOUT = Duration.ofHours(24)
        private val DEFAULT_DRAIN_TIMEOUT = Duration.ofSeconds(30)

        @JvmStatic
        fun builder(): Builder = Builder()
    }

    internal fun withMaxIdle(maxIdle: Int): PoolConfig {
        return PoolConfig(
            poolName = poolName,
            ownerId = ownerId,
            maxIdle = maxIdle,
            warmupConcurrency = warmupConcurrency,
            primaryLockTtl = primaryLockTtl,
            stateStore = stateStore,
            connectionConfig = connectionConfig,
            creationSpec = creationSpec,
            reconcileInterval = reconcileInterval,
            degradedThreshold = degradedThreshold,
            acquireReadyTimeout = acquireReadyTimeout,
            acquireHealthCheckPollingInterval = acquireHealthCheckPollingInterval,
            acquireHealthCheck = acquireHealthCheck,
            acquireSkipHealthCheck = acquireSkipHealthCheck,
            warmupReadyTimeout = warmupReadyTimeout,
            warmupHealthCheckPollingInterval = warmupHealthCheckPollingInterval,
            warmupHealthCheck = warmupHealthCheck,
            warmupSandboxPreparer = warmupSandboxPreparer,
            warmupSkipHealthCheck = warmupSkipHealthCheck,
            idleTimeout = idleTimeout,
            drainTimeout = drainTimeout,
        )
    }

    class Builder {
        private var poolName: String? = null
        private var ownerId: String? = null
        private var maxIdle: Int? = null
        private var warmupConcurrency: Int? = null
        private var primaryLockTtl: Duration = DEFAULT_PRIMARY_LOCK_TTL
        private var stateStore: PoolStateStore? = null
        private var connectionConfig: ConnectionConfig? = null
        private var creationSpec: PoolCreationSpec? = null
        private var reconcileInterval: Duration = DEFAULT_RECONCILE_INTERVAL
        private var degradedThreshold: Int = DEFAULT_DEGRADED_THRESHOLD
        private var acquireReadyTimeout: Duration = DEFAULT_ACQUIRE_READY_TIMEOUT
        private var acquireHealthCheckPollingInterval: Duration = DEFAULT_ACQUIRE_HEALTH_CHECK_POLLING_INTERVAL
        private var acquireHealthCheck: ((Sandbox) -> Boolean)? = null
        private var acquireSkipHealthCheck: Boolean = false
        private var warmupReadyTimeout: Duration = DEFAULT_WARMUP_READY_TIMEOUT
        private var warmupHealthCheckPollingInterval: Duration = DEFAULT_WARMUP_HEALTH_CHECK_POLLING_INTERVAL
        private var warmupHealthCheck: ((Sandbox) -> Boolean)? = null
        private var warmupSandboxPreparer: SandboxPreparer? = null
        private var warmupSkipHealthCheck: Boolean = false
        private var idleTimeout: Duration = DEFAULT_IDLE_TIMEOUT
        private var drainTimeout: Duration = DEFAULT_DRAIN_TIMEOUT

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

        fun reconcileInterval(reconcileInterval: Duration): Builder {
            this.reconcileInterval = reconcileInterval
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

        fun warmupReadyTimeout(warmupReadyTimeout: Duration): Builder {
            this.warmupReadyTimeout = warmupReadyTimeout
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

            val warmup = warmupConcurrency ?: ceil(max * 0.2).toInt().coerceAtLeast(1)

            return PoolConfig(
                poolName = name,
                ownerId = owner,
                maxIdle = max,
                warmupConcurrency = warmup,
                primaryLockTtl = primaryLockTtl,
                stateStore = store,
                connectionConfig = conn,
                creationSpec = spec,
                reconcileInterval = reconcileInterval,
                degradedThreshold = degradedThreshold,
                acquireReadyTimeout = acquireReadyTimeout,
                acquireHealthCheckPollingInterval = acquireHealthCheckPollingInterval,
                acquireHealthCheck = acquireHealthCheck,
                acquireSkipHealthCheck = acquireSkipHealthCheck,
                warmupReadyTimeout = warmupReadyTimeout,
                warmupHealthCheckPollingInterval = warmupHealthCheckPollingInterval,
                warmupHealthCheck = warmupHealthCheck,
                warmupSandboxPreparer = warmupSandboxPreparer,
                warmupSkipHealthCheck = warmupSkipHealthCheck,
                idleTimeout = idleTimeout,
                drainTimeout = drainTimeout,
            )
        }
    }
}
