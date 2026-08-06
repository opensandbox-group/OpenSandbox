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

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test

class WarmupPlanTest {
    @Test
    fun `empty pool fills all available warmup slots`() {
        val plan =
            WarmupPlan.calculate(
                idleCount = 0,
                warmingCount = 0,
                maxIdle = 10,
                warmupConcurrency = 3,
            )

        assertEquals(10, plan.deficit)
        assertEquals(3, plan.availableSlots)
        assertEquals(3, plan.toSubmit)
    }

    @Test
    fun `in-flight warmups count toward idle target and concurrency`() {
        val plan =
            WarmupPlan.calculate(
                idleCount = 4,
                warmingCount = 2,
                maxIdle = 8,
                warmupConcurrency = 3,
            )

        assertEquals(2, plan.deficit)
        assertEquals(1, plan.availableSlots)
        assertEquals(1, plan.toSubmit)
    }

    @Test
    fun `full warmup window submits no additional work`() {
        val plan =
            WarmupPlan.calculate(
                idleCount = 0,
                warmingCount = 4,
                maxIdle = 10,
                warmupConcurrency = 4,
            )

        assertEquals(6, plan.deficit)
        assertEquals(0, plan.availableSlots)
        assertEquals(0, plan.toSubmit)
    }

    @Test
    fun `satisfied or zero target submits no warmups`() {
        assertEquals(
            0,
            WarmupPlan.calculate(
                idleCount = 5,
                warmingCount = 0,
                maxIdle = 5,
                warmupConcurrency = 2,
            ).toSubmit,
        )
        assertEquals(
            0,
            WarmupPlan.calculate(
                idleCount = 0,
                warmingCount = 0,
                maxIdle = 0,
                warmupConcurrency = 1,
            ).toSubmit,
        )
    }
}
