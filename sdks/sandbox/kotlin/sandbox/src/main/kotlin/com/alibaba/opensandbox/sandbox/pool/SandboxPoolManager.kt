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

import com.alibaba.opensandbox.sandbox.SandboxManager
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolDestroyIncompleteException
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolDestroyedException
import com.alibaba.opensandbox.sandbox.domain.pool.PoolDestroyOptions
import com.alibaba.opensandbox.sandbox.domain.pool.PoolDestroyResult
import com.alibaba.opensandbox.sandbox.domain.pool.PoolDestroyState
import com.alibaba.opensandbox.sandbox.domain.pool.PoolDestroyStrategy
import com.alibaba.opensandbox.sandbox.domain.pool.PoolStateStore
import org.slf4j.LoggerFactory
import java.time.Instant
import java.util.UUID

/**
 * Administrative manager for shared sandbox pool namespaces.
 *
 * This manager does not acquire sandboxes. It performs namespace-level maintenance such as
 * destroying a distributed pool without requiring callers to construct the old SandboxPool
 * object that originally owned the namespace.
 */
class SandboxPoolManager
    @JvmOverloads
    constructor(
        private val stateStore: PoolStateStore,
        private val connectionConfig: ConnectionConfig,
        private val ownerId: String = "pool-manager-${UUID.randomUUID()}",
    ) {
        private val logger = LoggerFactory.getLogger(SandboxPoolManager::class.java)

        /**
         * Destroys a pool namespace.
         *
         * FORCE destroy writes a shared DESTROYING fence first. Running pool instances that
         * observe the fence must stop replenish/acquire paths instead of falling back to direct
         * create. The manager then drains visible idle IDs, best-effort kills them, clears
         * persistent coordination state, and writes a DESTROYED tombstone. If drain or cleanup
         * cannot finish, the namespace remains DESTROYING and callers should retry destroy.
         */
        @JvmOverloads
        fun destroy(
            poolName: String,
            options: PoolDestroyOptions = PoolDestroyOptions(),
        ): PoolDestroyResult {
            require(poolName.isNotBlank()) { "poolName must not be blank" }
            require(options.strategy == PoolDestroyStrategy.FORCE) {
                "Only FORCE destroy is supported in this version"
            }

            if (stateStore.getDestroyState(poolName) == PoolDestroyState.DESTROYED) {
                return destroyedResult(poolName)
            }

            val manager =
                SandboxManager.builder()
                    .connectionConfig(connectionConfig.copyWithoutConnectionPool())
                    .build()

            var drained = 0
            var killed = 0
            try {
                try {
                    stateStore.beginDestroy(poolName, ownerId)
                } catch (e: PoolDestroyedException) {
                    return destroyedResult(poolName)
                }
                val deadline = Instant.now().plus(options.drainTimeout)
                while (true) {
                    val sandboxId = stateStore.tryTakeIdle(poolName) ?: break
                    drained++
                    try {
                        manager.killSandbox(sandboxId)
                        killed++
                    } catch (e: Exception) {
                        logger.warn(
                            "Pool destroy failed to kill idle sandbox (best-effort): pool_name={} sandbox_id={} error={}",
                            poolName,
                            sandboxId,
                            e.message,
                        )
                    }
                    if (options.drainTimeout.toMillis() > 0 && Instant.now().isAfter(deadline)) {
                        throw PoolDestroyIncompleteException(
                            "Pool destroy drain timeout reached: poolName=$poolName drainedIdleCount=$drained",
                        )
                    }
                }
                try {
                    stateStore.clearPoolState(poolName)
                } catch (e: Exception) {
                    throw PoolDestroyIncompleteException(
                        message = "Pool destroy failed to clear persistent state: poolName=$poolName",
                        cause = e,
                    )
                }
                try {
                    stateStore.markDestroyed(poolName, ownerId, options.tombstoneTtl)
                } catch (e: Exception) {
                    throw PoolDestroyIncompleteException(
                        message = "Pool destroy failed to write destroyed tombstone: poolName=$poolName",
                        cause = e,
                    )
                }

                return PoolDestroyResult(
                    poolName = poolName,
                    state = PoolDestroyState.DESTROYED,
                    drainedIdleCount = drained,
                    killedIdleCount = killed,
                    persistentStateCleared = true,
                )
            } finally {
                manager.close()
            }
        }

        private fun destroyedResult(poolName: String): PoolDestroyResult =
            PoolDestroyResult(
                poolName = poolName,
                state = PoolDestroyState.DESTROYED,
                drainedIdleCount = 0,
                killedIdleCount = 0,
                persistentStateCleared = false,
            )

        companion object {
            @JvmStatic
            fun builder(): Builder = Builder()
        }

        class Builder internal constructor() {
            private var stateStore: PoolStateStore? = null
            private var connectionConfig: ConnectionConfig? = null
            private var ownerId: String? = null

            fun stateStore(stateStore: PoolStateStore): Builder {
                this.stateStore = stateStore
                return this
            }

            fun connectionConfig(connectionConfig: ConnectionConfig): Builder {
                this.connectionConfig = connectionConfig
                return this
            }

            fun ownerId(ownerId: String): Builder {
                this.ownerId = ownerId
                return this
            }

            fun build(): SandboxPoolManager {
                val store = stateStore ?: throw IllegalArgumentException("stateStore is required")
                val config = connectionConfig ?: throw IllegalArgumentException("connectionConfig is required")
                val owner = ownerId ?: "pool-manager-${UUID.randomUUID()}"
                require(owner.isNotBlank()) { "ownerId must not be blank" }
                return SandboxPoolManager(
                    stateStore = store,
                    connectionConfig = config,
                    ownerId = owner,
                )
            }
        }
    }
