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

package com.alibaba.opensandbox.sandbox

import com.alibaba.opensandbox.sandbox.domain.exceptions.InvalidArgumentException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxReadyTimeoutException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SnapshotFailedException
import com.alibaba.opensandbox.sandbox.domain.models.diagnostics.DiagnosticContent
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PagedSandboxInfos
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PaginationInfo
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxFilter
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxImageSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxInfo
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxRenewResponse
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxState
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxStatus
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SnapshotInfo
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SnapshotState
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SnapshotStatus
import com.alibaba.opensandbox.sandbox.domain.services.Diagnostics
import com.alibaba.opensandbox.sandbox.domain.services.Sandboxes
import io.mockk.Runs
import io.mockk.every
import io.mockk.impl.annotations.MockK
import io.mockk.junit5.MockKExtension
import io.mockk.just
import io.mockk.mockk
import io.mockk.verify
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertSame
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.extension.ExtendWith
import java.time.Duration
import java.time.OffsetDateTime

@ExtendWith(MockKExtension::class)
class SandboxManagerTest {
    @MockK
    lateinit var sandboxService: Sandboxes

    @MockK
    lateinit var diagnosticsService: Diagnostics

    @MockK
    lateinit var httpClientProvider: HttpClientProvider

    private lateinit var sandboxManager: SandboxManager

    @BeforeEach
    fun setUp() {
        sandboxManager = SandboxManager(sandboxService, httpClientProvider, diagnosticsService)
    }

    @Test
    fun `listSandboxInfos should return sandboxes from service`() {
        val filter = SandboxFilter.builder().states("RUNNING").build()
        val pagination =
            PaginationInfo(
                page = 1,
                pageSize = 10,
                totalItems = 2,
                totalPages = 1,
                hasNextPage = false,
            )
        val expectedInfos =
            PagedSandboxInfos(
                sandboxInfos = listOf(mockk(), mockk()),
                pagination = pagination,
            )

        every { sandboxService.listSandboxes(filter) } returns expectedInfos

        val result = sandboxManager.listSandboxInfos(filter)

        assertEquals(expectedInfos, result)
        verify { sandboxService.listSandboxes(filter) }
    }

    @Test
    fun `getSandboxInfo should return info from service`() {
        val sandboxId = "sandbox-id"
        val status =
            SandboxStatus(
                state = SandboxState.RUNNING,
                reason = null,
                message = null,
                lastTransitionAt = OffsetDateTime.now(),
            )
        val imageSpec = SandboxImageSpec.builder().image("ubuntu").build()
        val expectedInfo =
            SandboxInfo(
                id = sandboxId,
                status = status,
                entrypoint = listOf("/bin/bash"),
                createdAt = OffsetDateTime.now(),
                expiresAt = OffsetDateTime.now().plusHours(1),
                image = imageSpec,
                metadata = emptyMap(),
            )

        every { sandboxService.getSandboxInfo(sandboxId) } returns expectedInfo

        val result = sandboxManager.getSandboxInfo(sandboxId)

        assertEquals(expectedInfo, result)
        verify { sandboxService.getSandboxInfo(sandboxId) }
    }

    @Test
    fun `getDiagnosticLogs should return logs from diagnostics service`() {
        val sandboxId = "sandbox-id"
        val expected = mockk<DiagnosticContent>()
        every { diagnosticsService.getLogs(sandboxId, "container") } returns expected

        val result = sandboxManager.getDiagnosticLogs(sandboxId, "container")

        assertSame(expected, result)
        verify { diagnosticsService.getLogs(sandboxId, "container") }
    }

    @Test
    fun `getDiagnosticEvents should return events from diagnostics service`() {
        val sandboxId = "sandbox-id"
        val expected = mockk<DiagnosticContent>()
        every { diagnosticsService.getEvents(sandboxId, "runtime") } returns expected

        val result = sandboxManager.getDiagnosticEvents(sandboxId, "runtime")

        assertSame(expected, result)
        verify { diagnosticsService.getEvents(sandboxId, "runtime") }
    }

    @Test
    fun `killSandbox should call service`() {
        val sandboxId = "sandbox-id"
        every { sandboxService.killSandbox(sandboxId) } just Runs

        sandboxManager.killSandbox(sandboxId)

        verify { sandboxService.killSandbox(sandboxId) }
    }

    @Test
    fun `renewSandbox should call service`() {
        val sandboxId = "sandbox-id"
        val timeout = Duration.ofMinutes(30)
        val expectedRenew = mockk<SandboxRenewResponse>()

        every { sandboxService.renewSandboxExpiration(sandboxId, any()) } returns expectedRenew

        val actualRenew = sandboxManager.renewSandbox(sandboxId, timeout)

        assertSame(expectedRenew, actualRenew)
    }

    @Test
    fun `pauseSandbox should call service`() {
        val sandboxId = "sandbox-id"
        every { sandboxService.pauseSandbox(sandboxId) } just Runs

        sandboxManager.pauseSandbox(sandboxId)

        verify { sandboxService.pauseSandbox(sandboxId) }
    }

    @Test
    fun `resumeSandbox should call service`() {
        val sandboxId = "sandbox-id"
        every { sandboxService.resumeSandbox(sandboxId) } just Runs

        sandboxManager.resumeSandbox(sandboxId)

        verify { sandboxService.resumeSandbox(sandboxId) }
    }

    @Test
    fun `close should close httpClientProvider`() {
        every { httpClientProvider.close() } just Runs

        sandboxManager.close()

        verify { httpClientProvider.close() }
    }

    private fun snapshot(state: String): SnapshotInfo =
        SnapshotInfo(
            id = "snapshot-id",
            sandboxId = "sandbox-id",
            name = "snap",
            status = SnapshotStatus(state = state, reason = null, message = null, lastTransitionAt = null),
            createdAt = OffsetDateTime.now(),
        )

    @Test
    fun `waitForSnapshotReady returns once the snapshot becomes ready`() {
        val sequence = listOf(snapshot(SnapshotState.CREATING), snapshot(SnapshotState.READY))
        var index = 0
        every { sandboxService.getSnapshot("snapshot-id") } answers { sequence[index++] }

        val result =
            sandboxManager.waitForSnapshotReady(
                "snapshot-id",
                Duration.ofSeconds(5),
                Duration.ofMillis(10),
            )

        assertEquals(SnapshotState.READY, result.status.state)
        verify(exactly = 2) { sandboxService.getSnapshot("snapshot-id") }
    }

    @Test
    fun `waitForSnapshotReady throws SnapshotFailedException when the snapshot fails`() {
        every { sandboxService.getSnapshot("snapshot-id") } returns snapshot(SnapshotState.FAILED)

        assertThrows(SnapshotFailedException::class.java) {
            sandboxManager.waitForSnapshotReady("snapshot-id", Duration.ofSeconds(5), Duration.ofMillis(10))
        }
    }

    @Test
    fun `waitForSnapshotReady throws SandboxReadyTimeoutException when it never becomes ready`() {
        every { sandboxService.getSnapshot("snapshot-id") } returns snapshot(SnapshotState.CREATING)

        assertThrows(SandboxReadyTimeoutException::class.java) {
            sandboxManager.waitForSnapshotReady("snapshot-id", Duration.ofMillis(30), Duration.ofMillis(10))
        }
    }

    @Test
    fun `waitForSnapshotReady rejects a non-positive polling interval`() {
        assertThrows(InvalidArgumentException::class.java) {
            sandboxManager.waitForSnapshotReady("snapshot-id", Duration.ofSeconds(5), Duration.ZERO)
        }
    }

    @Test
    fun `waitForSnapshotReady keeps polling within the window instead of giving up early`() {
        // Several non-ready polls within a generous window must not trigger a premature timeout.
        val sequence =
            listOf(
                snapshot(SnapshotState.CREATING),
                snapshot(SnapshotState.CREATING),
                snapshot(SnapshotState.READY),
            )
        var index = 0
        every { sandboxService.getSnapshot("snapshot-id") } answers { sequence[index++] }

        val result =
            sandboxManager.waitForSnapshotReady(
                "snapshot-id",
                Duration.ofSeconds(1),
                Duration.ofMillis(20),
            )

        assertEquals(SnapshotState.READY, result.status.state)
        verify(exactly = 3) { sandboxService.getSnapshot("snapshot-id") }
    }

    @Test
    fun `waitForSnapshotReady does not accept a snapshot that turns ready only after the deadline`() {
        // The interval (100ms) outlasts the timeout (80ms): after the single sleep the deadline has
        // passed, so the late READY must be rejected with a timeout rather than returned as success.
        val sequence = listOf(snapshot(SnapshotState.CREATING), snapshot(SnapshotState.READY))
        var index = 0
        every { sandboxService.getSnapshot("snapshot-id") } answers { sequence[index++] }

        assertThrows(SandboxReadyTimeoutException::class.java) {
            sandboxManager.waitForSnapshotReady("snapshot-id", Duration.ofMillis(80), Duration.ofMillis(100))
        }
    }

    @Test
    fun `waitForSnapshotReady rejects a READY response that blocks past the deadline`() {
        // Each poll blocks ~80ms; with a 100ms timeout the READY response is produced only after the
        // deadline elapses, so it must surface as a timeout rather than a late success.
        val sequence = listOf(snapshot(SnapshotState.CREATING), snapshot(SnapshotState.READY))
        var index = 0
        every { sandboxService.getSnapshot("snapshot-id") } answers {
            Thread.sleep(80)
            sequence[index++]
        }

        assertThrows(SandboxReadyTimeoutException::class.java) {
            sandboxManager.waitForSnapshotReady("snapshot-id", Duration.ofMillis(100), Duration.ofMillis(10))
        }
    }
}
