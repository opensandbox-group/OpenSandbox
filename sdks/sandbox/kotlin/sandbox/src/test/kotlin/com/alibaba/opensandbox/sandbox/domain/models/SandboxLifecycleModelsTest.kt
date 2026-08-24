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

package com.alibaba.opensandbox.sandbox.domain.models

import com.alibaba.opensandbox.sandbox.api.models.CreateSandboxRequest
import com.alibaba.opensandbox.sandbox.api.models.LifecycleHook
import com.alibaba.opensandbox.sandbox.api.models.PeriodicLifecycleHook
import com.alibaba.opensandbox.sandbox.api.models.SandboxLifecycle
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxImageSpec
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.LifecycleHook as DomainLifecycleHook
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PeriodicLifecycleHook as DomainPeriodicLifecycleHook
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxLifecycle as DomainSandboxLifecycle

class SandboxLifecycleModelsTest {
    @Test
    fun `stable lifecycle builders reject timeout above maximum`() {
        assertEquals(300, DomainLifecycleHook.builder().command("true").timeoutSeconds(300).build().timeoutSeconds)

        assertThrows(IllegalArgumentException::class.java) {
            DomainLifecycleHook.builder().command("true").timeoutSeconds(301)
        }
        assertThrows(IllegalArgumentException::class.java) {
            DomainPeriodicLifecycleHook.builder()
                .name("sync")
                .schedule("@hourly")
                .command("true")
                .timeoutSeconds(301)
        }
    }

    @Test
    fun `stable lifecycle builders reject blank commands and normalize periodic text`() {
        assertThrows(IllegalArgumentException::class.java) {
            DomainLifecycleHook.builder().command(" ")
        }
        assertThrows(IllegalArgumentException::class.java) {
            DomainPeriodicLifecycleHook.builder().name(" ")
        }
        assertThrows(IllegalArgumentException::class.java) {
            DomainPeriodicLifecycleHook.builder().command(" ")
        }

        val periodic =
            DomainPeriodicLifecycleHook.builder()
                .name(" sync ")
                .schedule(" @hourly ")
                .command("true")
                .build()

        assertEquals("sync", periodic.name)
        assertEquals("@hourly", periodic.schedule)
    }

    @Test
    fun `stable lifecycle builders snapshot mutable command and periodic lists`() {
        val command = mutableListOf("true")
        val periodic =
            mutableListOf(
                DomainPeriodicLifecycleHook.builder()
                    .name("sync")
                    .schedule("@hourly")
                    .command(command)
                    .build(),
            )
        val lifecycle = DomainSandboxLifecycle.builder().periodic(periodic).build()

        command.clear()
        periodic.clear()

        assertEquals(listOf("true"), lifecycle.periodic?.single()?.command)
    }

    @Test
    fun `empty stable lifecycle is omitted from create request`() {
        val request =
            SandboxModelConverter.toApiCreateSandboxRequest(
                spec = SandboxImageSpec.builder().image("python:3.11").build(),
                entrypoint = listOf("python"),
                env = emptyMap(),
                metadata = emptyMap(),
                timeout = null,
                resource = emptyMap(),
                platform = null,
                networkPolicy = null,
                credentialProxy = null,
                secureAccess = false,
                extensions = emptyMap(),
                volumes = null,
                snapshotId = null,
                lifecycle = DomainSandboxLifecycle.builder().build(),
            )

        assertEquals(null, request.lifecycle)
    }

    @Test
    fun `empty periodic list is omitted when preStart is configured`() {
        val lifecycle =
            DomainSandboxLifecycle.builder()
                .preStart(DomainLifecycleHook.builder().command("true").build())
                .periodic(emptyList())
                .build()

        assertEquals(null, lifecycle.periodic)
    }

    @Test
    fun `create request lifecycle round trips through JSON`() {
        val request =
            CreateSandboxRequest(
                lifecycle =
                    SandboxLifecycle(
                        preStart = LifecycleHook(command = listOf("/opt/hooks/restore.sh")),
                        periodic =
                            listOf(
                                PeriodicLifecycleHook(
                                    name = "checkpoint",
                                    schedule = "*/5 * * * *",
                                    command = listOf("/opt/hooks/checkpoint.sh"),
                                ),
                            ),
                    ),
            )

        val decoded = Json.decodeFromString<CreateSandboxRequest>(Json.encodeToString(request))

        assertEquals(request.lifecycle, decoded.lifecycle)
    }

    @Test
    fun `stable lifecycle models map into create request`() {
        val request =
            SandboxModelConverter.toApiCreateSandboxRequest(
                spec = SandboxImageSpec.builder().image("python:3.11").build(),
                entrypoint = listOf("python"),
                env = emptyMap(),
                metadata = emptyMap(),
                timeout = null,
                resource = emptyMap(),
                platform = null,
                networkPolicy = null,
                credentialProxy = null,
                secureAccess = false,
                extensions = emptyMap(),
                volumes = null,
                snapshotId = null,
                lifecycle =
                    DomainSandboxLifecycle.builder()
                        .preStart(
                            DomainLifecycleHook.builder()
                                .command("/opt/hooks/restore.sh")
                                .timeoutSeconds(30)
                                .build(),
                        )
                        .periodic(
                            DomainPeriodicLifecycleHook.builder()
                                .name("checkpoint")
                                .schedule("*/5 * * * *")
                                .command("/opt/hooks/checkpoint.sh")
                                .build(),
                        )
                        .build(),
            )

        assertEquals(listOf("/opt/hooks/restore.sh"), request.lifecycle?.preStart?.command)
        assertEquals(30, request.lifecycle?.preStart?.timeoutSeconds)
        assertEquals("checkpoint", request.lifecycle?.periodic?.single()?.name)
    }
}
