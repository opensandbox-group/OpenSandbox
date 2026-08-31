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
    fun `empty pool admits at most one second of create qps`() {
        val plan =
            WarmupPlan.calculate(
                idleCount = 0,
                warmingCount = 0,
                maxIdle = 10,
                warmupCreateQps = 3,
            )

        assertEquals(10, plan.deficit)
        assertEquals(3, plan.toSubmit)
    }

    @Test
    fun `in-flight warmups count toward idle target`() {
        val plan =
            WarmupPlan.calculate(
                idleCount = 4,
                warmingCount = 2,
                maxIdle = 8,
                warmupCreateQps = 3,
            )

        assertEquals(2, plan.deficit)
        assertEquals(2, plan.toSubmit)
    }

    @Test
    fun `large in-flight count does not independently cap admission`() {
        val plan =
            WarmupPlan.calculate(
                idleCount = 0,
                warmingCount = 4,
                maxIdle = 10,
                warmupCreateQps = 4,
            )

        assertEquals(6, plan.deficit)
        assertEquals(4, plan.toSubmit)
    }

    @Test
    fun `satisfied or zero target submits no warmups`() {
        assertEquals(
            0,
            WarmupPlan.calculate(
                idleCount = 5,
                warmingCount = 0,
                maxIdle = 5,
                warmupCreateQps = 2,
            ).toSubmit,
        )
        assertEquals(
            0,
            WarmupPlan.calculate(
                idleCount = 0,
                warmingCount = 0,
                maxIdle = 0,
                warmupCreateQps = 1,
            ).toSubmit,
        )
    }
}
