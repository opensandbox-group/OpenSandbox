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

package com.alibaba.opensandbox.sandbox

import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.CreateIsolatedSessionRequest
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedCapabilities
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedRunRequest
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedSessionInfo
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedSessionSummary
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedWorkspaceSpec
import com.alibaba.opensandbox.sandbox.domain.services.IsolationService
import com.alibaba.opensandbox.sandbox.domain.services.IsolationSession
import io.mockk.Runs
import io.mockk.every
import io.mockk.just
import io.mockk.mockk
import io.mockk.verify
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.assertThrows
import java.time.OffsetDateTime

class IsolationRunOnceTest {
    private fun mockSession(sessionId: String = "sess-1"): IsolationSession {
        val session = mockk<IsolationSession>()
        every { session.sessionId } returns sessionId
        every { session.info } returns
            IsolatedSessionInfo(
                sessionId = sessionId,
                createdAt = OffsetDateTime.now(),
            )
        every { session.run(any<IsolatedRunRequest>()) } returns Execution()
        every { session.delete() } just Runs
        return session
    }

    private fun mockService(session: IsolationSession): IsolationService {
        return object : IsolationService {
            override fun create(request: CreateIsolatedSessionRequest): IsolationSession = session

            override fun capabilities(): IsolatedCapabilities = IsolatedCapabilities()

            override fun list(): List<IsolatedSessionSummary> = emptyList()
        }
    }

    @Test
    fun `runOnce creates runs and deletes`() {
        val session = mockSession()
        val service = mockService(session)

        val result = service.runOnce("echo hello", "/workspace")

        verify(exactly = 1) { session.run(any<IsolatedRunRequest>()) }
        verify(exactly = 1) { session.delete() }
        assert(result != null)
    }

    @Test
    fun `runOnce deletes on run failure`() {
        val session = mockSession()
        every { session.run(any<IsolatedRunRequest>()) } throws RuntimeException("boom")
        val service = mockService(session)

        assertThrows<RuntimeException> {
            service.runOnce("bad", "/workspace")
        }

        verify(exactly = 1) { session.delete() }
    }

    @Test
    fun `runOnce tolerates delete failure`() {
        val session = mockSession()
        every { session.delete() } throws RuntimeException("delete failed")
        val service = mockService(session)

        val result = service.runOnce("echo ok", "/workspace")
        assert(result != null)
    }

    @Test
    fun `withSession calls block and deletes`() {
        val session = mockSession()
        val service = mockService(session)
        var blockCalled = false

        val result =
            service.withSession(
                CreateIsolatedSessionRequest(workspace = IsolatedWorkspaceSpec(path = "/workspace")),
            ) { s ->
                blockCalled = true
                assert(s.sessionId == "sess-1")
                "done"
            }

        assert(blockCalled)
        assert(result == "done")
        verify(exactly = 1) { session.delete() }
    }

    @Test
    fun `withSession deletes on block exception`() {
        val session = mockSession()
        val service = mockService(session)

        assertThrows<IllegalStateException> {
            service.withSession(
                CreateIsolatedSessionRequest(workspace = IsolatedWorkspaceSpec(path = "/workspace")),
            ) {
                throw IllegalStateException("block error")
            }
        }

        verify(exactly = 1) { session.delete() }
    }
}
