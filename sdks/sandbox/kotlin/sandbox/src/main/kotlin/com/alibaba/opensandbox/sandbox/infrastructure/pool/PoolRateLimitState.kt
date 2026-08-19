/*
 * Copyright 2026 Alibaba Group Holding Ltd.
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

import com.alibaba.opensandbox.sandbox.transport.RETRY_AFTER_CAP
import java.time.Duration
import java.time.Instant

/** Per-run warmup throttle established by rate-limited sandbox creates. */
internal class PoolRateLimitState(
    private val defaultDelay: Duration = DEFAULT_RATE_LIMIT_DELAY,
    private val maxDelay: Duration = RETRY_AFTER_CAP,
) {
    init {
        require(!defaultDelay.isNegative) { "defaultDelay must not be negative" }
        require(!maxDelay.isNegative) { "maxDelay must not be negative" }
    }

    @Volatile
    private var throttleUntil: Instant? = null

    /** Extends, but never shortens, the current throttle deadline. */
    @Synchronized
    fun recordRateLimit(
        retryAfter: Duration?,
        now: Instant = Instant.now(),
    ) {
        val requestedDelay = retryAfter?.takeUnless { it.isNegative || it.isZero } ?: defaultDelay
        val candidate = now.plus(minOf(requestedDelay, maxDelay))
        val current = throttleUntil
        if (current == null || candidate.isAfter(current)) {
            throttleUntil = candidate
        }
    }

    fun isActive(now: Instant = Instant.now()): Boolean {
        val until = throttleUntil ?: return false
        return now.isBefore(until)
    }

    fun remainingDelay(now: Instant = Instant.now()): Duration {
        val until = throttleUntil ?: return Duration.ZERO
        val remaining = Duration.between(now, until)
        return if (remaining.isNegative || remaining.isZero) Duration.ZERO else remaining
    }

    companion object {
        internal val DEFAULT_RATE_LIMIT_DELAY: Duration = Duration.ofSeconds(10)
    }
}
