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

package com.alibaba.opensandbox.sandbox.domain.models

import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PlatformSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxImageSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxInfo
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxState
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxStatus
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test
import java.time.OffsetDateTime

class SandboxInfoCompatibilityTest {
    @Test
    fun `SandboxInfo retains the pre-allocation JVM constructor`() {
        val constructor =
            SandboxInfo::class.java.getConstructor(
                String::class.java,
                SandboxStatus::class.java,
                List::class.java,
                OffsetDateTime::class.java,
                OffsetDateTime::class.java,
                SandboxImageSpec::class.java,
                String::class.java,
                PlatformSpec::class.java,
                Map::class.java,
                Map::class.java,
            )
        val createdAt = OffsetDateTime.parse("2026-07-10T00:00:00Z")
        val status = SandboxStatus(SandboxState.RUNNING, null, null, createdAt)

        val sandboxInfo =
            constructor.newInstance(
                "sandbox-id",
                status,
                listOf("/bin/sh"),
                null,
                createdAt,
                null,
                null,
                null,
                null,
                null,
            )

        assertEquals("sandbox-id", sandboxInfo.id)
        assertEquals(status, sandboxInfo.status)
        assertNull(sandboxInfo.allocation)
    }
}
