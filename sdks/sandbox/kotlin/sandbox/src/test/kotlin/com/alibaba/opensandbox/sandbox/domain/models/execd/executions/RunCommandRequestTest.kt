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

package com.alibaba.opensandbox.sandbox.domain.models.execd.executions

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test
import java.time.Duration
import kotlin.time.Duration.Companion.seconds

class RunCommandRequestTest {
    @Test
    fun `builder accepts java duration for timeout`() {
        val request =
            RunCommandRequest.builder()
                .command("echo hi")
                .timeout(Duration.ofSeconds(5))
                .build()

        assertEquals(Duration.ofSeconds(5), request.timeout)
    }

    @Suppress("DEPRECATION")
    @Test
    fun `builder accepts deprecated kotlin duration for timeout`() {
        val request =
            RunCommandRequest.builder()
                .command("echo hi")
                .timeout(5.seconds)
                .build()

        assertEquals(Duration.ofSeconds(5), request.timeout)
    }
}
