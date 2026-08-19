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

/**
 * Point-in-time replenish plan for the rolling warmup window.
 *
 * Both idle sandboxes and already-admitted warmups count toward the target so repeated
 * reconcile ticks cannot over-submit while earlier warmups are still running.
 */
internal data class WarmupPlan(
    val idleCount: Int,
    val warmingCount: Int,
    val maxIdle: Int,
    val warmupConcurrency: Int,
    val deficit: Int,
    val availableSlots: Int,
    val toSubmit: Int,
) {
    companion object {
        fun calculate(
            idleCount: Int,
            warmingCount: Int,
            maxIdle: Int,
            warmupConcurrency: Int,
        ): WarmupPlan {
            require(idleCount >= 0) { "idleCount must be >= 0" }
            require(warmingCount >= 0) { "warmingCount must be >= 0" }
            require(maxIdle >= 0) { "maxIdle must be >= 0" }
            require(warmupConcurrency > 0) { "warmupConcurrency must be positive" }

            val deficit = (maxIdle - idleCount - warmingCount).coerceAtLeast(0)
            val availableSlots = (warmupConcurrency - warmingCount).coerceAtLeast(0)
            return WarmupPlan(
                idleCount = idleCount,
                warmingCount = warmingCount,
                maxIdle = maxIdle,
                warmupConcurrency = warmupConcurrency,
                deficit = deficit,
                availableSlots = availableSlots,
                toSubmit = minOf(deficit, availableSlots),
            )
        }
    }
}
