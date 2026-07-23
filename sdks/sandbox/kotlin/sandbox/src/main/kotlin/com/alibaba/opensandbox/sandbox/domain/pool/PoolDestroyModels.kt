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

/**
 * Shared destroy lifecycle for one pool namespace.
 */
enum class PoolDestroyState {
    ACTIVE,
    DESTROYING,
    DESTROYED,
}

/**
 * Destroy strategy. V1 implements FORCE only; the enum leaves room for a future
 * safe strategy that rejects destroy when active pool members are present.
 */
enum class PoolDestroyStrategy {
    FORCE,
}

/**
 * Options for destroying a pool namespace through SandboxPoolManager.
 */
class PoolDestroyOptions
    @JvmOverloads
    constructor(
        val strategy: PoolDestroyStrategy = PoolDestroyStrategy.FORCE,
        val tombstoneTtl: Duration? = Duration.ofDays(7),
        val drainTimeout: Duration = Duration.ofSeconds(30),
    ) {
        init {
            require(tombstoneTtl == null || (!tombstoneTtl.isNegative && !tombstoneTtl.isZero)) {
                "tombstoneTtl must be positive when set"
            }
            require(!drainTimeout.isNegative) { "drainTimeout must be non-negative" }
        }
    }

/**
 * Result of a pool namespace destroy operation.
 */
class PoolDestroyResult(
    val poolName: String,
    val state: PoolDestroyState,
    val drainedIdleCount: Int,
    val killedIdleCount: Int,
    val persistentStateCleared: Boolean,
)
