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

import com.alibaba.opensandbox.sandbox.domain.pool.PoolState
import java.time.Duration
import java.time.Instant

/**
 * Mutable state for reconcile loop: failure count, pool state, and exponential backoff.
 *
 * Thread-safe for use from reconcile worker and from pool snapshot.
 */
internal class ReconcileState(
    private val degradedThreshold: Int,
    private val backoffBase: Duration = Duration.ofSeconds(30),
    private val backoffMax: Duration = Duration.ofDays(1),
) {
    @Volatile
    var failureCount: Int = 0
        private set

    @Volatile
    var state: PoolState = PoolState.HEALTHY
        private set

    @Volatile
    var lastError: String? = null
        private set

    @Volatile
    private var backoffUntil: Instant? = null

    private var backoffAttempts: Int = 0

    @Synchronized
    fun recordSuccess() {
        failureCount = 0
        if (state == PoolState.DEGRADED) state = PoolState.HEALTHY
        backoffUntil = null
        backoffAttempts = 0
        lastError = null
    }

    @Synchronized
    fun recordFailure(errorMessage: String?) {
        recordFailures(1, errorMessage)
    }

    @Synchronized
    fun recordFailures(
        count: Int,
        errorMessage: String?,
    ) {
        if (count <= 0) return
        failureCount += count
        lastError = errorMessage
        if (failureCount >= degradedThreshold) {
            state = PoolState.DEGRADED
            backoffAttempts++
            val exponent = (backoffAttempts - 1).coerceAtMost(30)
            val delaySeconds = backoffBase.seconds * (1L shl exponent)
            val delayMs =
                minOf(
                    Duration.ofSeconds(delaySeconds).toMillis(),
                    backoffMax.toMillis(),
                )
            backoffUntil = Instant.now().plusMillis(delayMs)
        }
    }

    /** True if reconciler should skip create attempts this tick (in backoff window). */
    fun isBackoffActive(now: Instant = Instant.now()): Boolean {
        val until = backoffUntil ?: return false
        return state == PoolState.DEGRADED && now.isBefore(until)
    }
}
