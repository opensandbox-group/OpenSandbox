/*
 * Copyright 2026 The OpenSandbox Authors
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
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.ActivityProbe
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.IdlePolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.OnIdleAction
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.RenewalPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxImageSpec
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test
import com.alibaba.opensandbox.sandbox.api.models.ActivityProbe as ApiActivityProbe
import com.alibaba.opensandbox.sandbox.api.models.IdlePolicy as ApiIdlePolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.LifecycleHook as DomainLifecycleHook
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PeriodicLifecycleHook as DomainPeriodicLifecycleHook
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxLifecycle as DomainSandboxLifecycle

class SandboxLifecycleModelsTest {
    @Test
    fun `stable lifecycle builders preserve timeout for server validation`() {
        assertEquals(0, DomainLifecycleHook.builder().command("true").timeoutSeconds(0).build().timeoutSeconds)
        assertEquals(
            301,
            DomainPeriodicLifecycleHook.builder()
                .name("sync")
                .schedule("@hourly")
                .command("true")
                .timeoutSeconds(301)
                .build()
                .timeoutSeconds,
        )
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

    @Test
    fun `idle policy validation bounds and pairing`() {
        assertThrows(IllegalArgumentException::class.java) {
            IdlePolicy.builder().idleTimeoutSeconds(29)
        }
        assertThrows(IllegalArgumentException::class.java) {
            IdlePolicy.builder().idleTimeoutSeconds(86401)
        }
        // onIdle without a timeout is wake-only invalid pairing.
        assertThrows(IllegalArgumentException::class.java) {
            IdlePolicy.builder().onIdle(OnIdleAction.PAUSE).build()
        }
        // timeout without onIdle is rejected.
        assertThrows(IllegalArgumentException::class.java) {
            IdlePolicy.builder().idleTimeoutSeconds(300).build()
        }
        // activeProbe requires a timeout (auto-pause).
        assertThrows(IllegalArgumentException::class.java) {
            IdlePolicy.builder()
                .wakeOnAccess(false)
                .activeProbe(ActivityProbe.builder().port(8080).path("/active").build())
                .build()
        }
    }

    @Test
    fun `empty idle policy builder fails fast`() {
        val error =
            assertThrows(IllegalArgumentException::class.java) {
                IdlePolicy.builder().build()
            }
        assertEquals("IdlePolicy must set at least one field", error.message)
    }

    @Test
    fun `renewal policy validation bounds and required field`() {
        assertThrows(IllegalArgumentException::class.java) {
            RenewalPolicy.builder().extendSeconds(299).build()
        }
        assertThrows(IllegalArgumentException::class.java) {
            RenewalPolicy.builder().extendSeconds(86401).build()
        }
        assertThrows(IllegalArgumentException::class.java) {
            RenewalPolicy.builder().build()
        }
        assertEquals(300, RenewalPolicy.builder().extendSeconds(300).build().extendSeconds)
        assertEquals(86400, RenewalPolicy.builder().extendSeconds(86400).build().extendSeconds)
    }

    @Test
    fun `renewal policy maps into create request`() {
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
                        .renewalPolicy { extendSeconds(3600) }
                        .build(),
            )

        assertEquals(3600, request.lifecycle?.renewalPolicy?.extendSeconds)
    }

    @Test
    fun `renewal-only lifecycle is not treated as empty`() {
        val lifecycle =
            DomainSandboxLifecycle.builder()
                .renewalPolicy { extendSeconds(3600) }
                .build()

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
                lifecycle = lifecycle,
            )

        // A renewal-only lifecycle must reach the wire, not be dropped as empty.
        assertEquals(3600, request.lifecycle?.renewalPolicy?.extendSeconds)
    }

    @Test
    fun `activity probe validation`() {
        assertThrows(IllegalArgumentException::class.java) {
            ActivityProbe.builder().port(0).path("/active").build()
        }
        assertThrows(IllegalArgumentException::class.java) {
            ActivityProbe.builder().port(65536).path("/active").build()
        }
        assertThrows(IllegalArgumentException::class.java) {
            ActivityProbe.builder().port(8080).path("active").build()
        }
        assertThrows(IllegalArgumentException::class.java) {
            ActivityProbe.builder().port(8080).path("/a b").build()
        }
        assertThrows(IllegalArgumentException::class.java) {
            ActivityProbe.builder().port(8080).path("/active").timeoutSeconds(11).build()
        }
    }

    @Test
    fun `idle policy maps into create request lifecycle`() {
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
                        .idlePolicy(
                            IdlePolicy.builder()
                                .idleTimeoutSeconds(300)
                                .onIdle(OnIdleAction.PAUSE)
                                .wakeOnAccess(true)
                                .activeProbe(
                                    ActivityProbe.builder()
                                        .port(8080)
                                        .path("/active")
                                        .timeoutSeconds(2)
                                        .build(),
                                )
                                .build(),
                        )
                        .build(),
            )

        val apiIdlePolicy = request.lifecycle?.idlePolicy
        assertEquals(300, apiIdlePolicy?.idleTimeoutSeconds)
        assertEquals(ApiIdlePolicy.OnIdle.pause, apiIdlePolicy?.onIdle)
        assertEquals(true, apiIdlePolicy?.wakeOnAccess)
        assertEquals(8080, apiIdlePolicy?.activeProbe?.port)
        assertEquals("/active", apiIdlePolicy?.activeProbe?.path)
        assertEquals(2, apiIdlePolicy?.activeProbe?.timeoutSeconds)
    }

    @Test
    fun `idle policy round trips through JSON`() {
        val lifecycle =
            SandboxLifecycle(
                idlePolicy =
                    ApiIdlePolicy(
                        idleTimeoutSeconds = 300,
                        onIdle = ApiIdlePolicy.OnIdle.pause,
                        wakeOnAccess = true,
                        activeProbe =
                            ApiActivityProbe(
                                port = 8080,
                                path = "/active",
                                timeoutSeconds = 2,
                            ),
                    ),
            )

        val decoded =
            Json.decodeFromString<CreateSandboxRequest>(
                Json.encodeToString(CreateSandboxRequest(lifecycle = lifecycle)),
            )

        assertEquals(lifecycle, decoded.lifecycle)
    }
}
