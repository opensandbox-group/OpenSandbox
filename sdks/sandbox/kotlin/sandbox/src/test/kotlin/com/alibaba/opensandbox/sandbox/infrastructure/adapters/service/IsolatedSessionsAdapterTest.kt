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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.service

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.CreateIsolatedSessionRequest
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedWorkspaceSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.long
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

class IsolatedSessionsAdapterTest {
    private lateinit var mockWebServer: MockWebServer
    private lateinit var adapter: IsolatedSessionsAdapter
    private lateinit var httpClientProvider: HttpClientProvider

    @BeforeEach
    fun setUp() {
        mockWebServer = MockWebServer()
        mockWebServer.start()

        val host = mockWebServer.hostName
        val port = mockWebServer.port
        val endpoint =
            SandboxEndpoint(
                endpoint = "$host:$port",
                headers = mapOf("X-Execd-Token" to "route-token"),
            )

        val config =
            ConnectionConfig.builder()
                .domain("$host:$port")
                .protocol("http")
                .build()

        httpClientProvider = HttpClientProvider(config)
        adapter = IsolatedSessionsAdapter(httpClientProvider, endpoint)
    }

    @AfterEach
    fun tearDown() {
        mockWebServer.shutdown()
        httpClientProvider.close()
    }

    @Test
    fun `list maps isolated session summaries`() {
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "sessions": [
                        {
                          "session_id": "sess-1",
                          "status": "idle",
                          "created_at": "2026-01-02T03:04:05Z",
                          "last_run_at": "2026-01-02T03:05:06Z",
                          "idle_remaining_seconds": 42
                        },
                        {
                          "session_id": "sess-2",
                          "status": "running",
                          "created_at": "2026-01-02T03:06:07Z",
                          "last_run_at": null,
                          "idle_remaining_seconds": null
                        }
                      ]
                    }
                    """.trimIndent(),
                ),
        )

        val sessions = adapter.list()

        val request = mockWebServer.takeRequest()
        assertEquals("GET", request.method)
        assertEquals("/v1/isolated/sessions", request.path)
        assertEquals("route-token", request.getHeader("X-Execd-Token"))

        assertEquals(2, sessions.size)

        val first = sessions[0]
        assertEquals("sess-1", first.sessionId)
        assertEquals("idle", first.status)
        assertEquals(2026, first.createdAt?.year)
        assertEquals(5, first.lastRunAt?.minute)
        assertEquals(42, first.idleRemainingSeconds)

        val second = sessions[1]
        assertEquals("sess-2", second.sessionId)
        assertEquals("running", second.status)
        assertNull(second.lastRunAt)
        assertNull(second.idleRemainingSeconds)
    }

    @Test
    fun `list returns empty when no sessions`() {
        mockWebServer.enqueue(MockResponse().setBody("""{"sessions": []}"""))

        val sessions = adapter.list()

        assertEquals(0, sessions.size)
        assertEquals("/v1/isolated/sessions", mockWebServer.takeRequest().path)
    }

    @Test
    fun `create serializes uid and gid above Int MaxValue`() {
        // Spec declares uid/gid as uint32; values above Int.MAX_VALUE must not fail.
        val uidAboveInt = 3_000_000_000L
        val gidAboveInt = 4_000_000_000L
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "session_id": "00000000-0000-0000-0000-000000000001",
                      "created_at": "2026-01-02T03:04:05Z"
                    }
                    """.trimIndent(),
                ),
        )

        adapter.create(
            CreateIsolatedSessionRequest(
                workspace = IsolatedWorkspaceSpec(path = "/workspace"),
                uid = uidAboveInt,
                gid = gidAboveInt,
            ),
        )

        val request = mockWebServer.takeRequest()
        assertEquals("POST", request.method)
        assertEquals("/v1/isolated/session", request.path)
        val body = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
        assertEquals(uidAboveInt, body["uid"]!!.jsonPrimitive.long)
        assertEquals(gidAboveInt, body["gid"]!!.jsonPrimitive.long)
    }

    @Test
    fun `attach populatesFullInfo whenExecdReturnsAllFields`() {
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "status": "active",
                      "created_at": "2026-01-02T03:04:05Z",
                      "last_run_at": "2026-01-02T03:05:06Z",
                      "idle_remaining_seconds": 30,
                      "profile": "strict",
                      "workspace": {"path": "/workspace", "mode": "rw"},
                      "extra_writable": ["/tmp", "/var/tmp"],
                      "binds": [
                        {"source": "/host/a", "dest": "/sbx/a", "readonly": true}
                      ],
                      "share_net": false,
                      "env_passthrough": {"mode": "allow", "keys": ["PATH", "HOME"]},
                      "uid": 1000,
                      "gid": 2000,
                      "uid_mode": "userns",
                      "idle_timeout_seconds": 300
                    }
                    """.trimIndent(),
                ),
        )

        val session = adapter.attach("sess-full")

        val request = mockWebServer.takeRequest()
        assertEquals("GET", request.method)
        assertEquals("/v1/isolated/session/sess-full", request.path)
        assertEquals("route-token", request.getHeader("X-Execd-Token"))

        assertEquals("sess-full", session.sessionId)
        val info = session.info
        assertEquals("sess-full", info.sessionId)
        assertEquals(2026, info.createdAt?.year)
        assertEquals("strict", info.profile)
        assertEquals("/workspace", info.workspace?.path)
        assertEquals("rw", info.workspace?.mode)
        assertEquals(listOf("/tmp", "/var/tmp"), info.extraWritable)
        assertEquals(1, info.binds?.size)
        assertEquals("/host/a", info.binds?.get(0)?.source)
        assertEquals("/sbx/a", info.binds?.get(0)?.dest)
        assertEquals(true, info.binds?.get(0)?.readonly)
        assertEquals(false, info.shareNet)
        assertEquals("allow", info.envPassthrough?.mode)
        assertEquals(listOf("PATH", "HOME"), info.envPassthrough?.keys)
        assertEquals(1000, info.uid)
        assertEquals(2000, info.gid)
        assertEquals("userns", info.uidMode)
        assertEquals(300, info.idleTimeoutSeconds)
    }

    @Test
    fun `attach toleratesMissingCreationParams whenExecdIsOlder`() {
        // GET returns only the runtime status fields — older execd behavior.
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "status": "active",
                      "created_at": "2026-01-02T03:04:05Z",
                      "last_run_at": null,
                      "idle_remaining_seconds": null
                    }
                    """.trimIndent(),
                ),
        )
        // Follow-up GET for session.get()
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "status": "active",
                      "created_at": "2026-01-02T03:04:05Z",
                      "last_run_at": null,
                      "idle_remaining_seconds": 7
                    }
                    """.trimIndent(),
                ),
        )
        // DELETE
        mockWebServer.enqueue(MockResponse().setResponseCode(204))

        val session = adapter.attach("sess-old")

        assertEquals("/v1/isolated/session/sess-old", mockWebServer.takeRequest().path)

        val info = session.info
        assertEquals("sess-old", info.sessionId)
        assertNotNull(info.createdAt)
        assertNull(info.profile)
        assertNull(info.workspace)
        assertNull(info.extraWritable)
        assertNull(info.binds)
        assertNull(info.shareNet)
        assertNull(info.envPassthrough)
        assertNull(info.uid)
        assertNull(info.gid)
        assertNull(info.uidMode)
        assertNull(info.idleTimeoutSeconds)

        // get() must still work by session id even when creation params were absent.
        val state = session.get()
        assertEquals("active", state.status)
        assertEquals(7, state.idleRemainingSeconds)
        val getRequest = mockWebServer.takeRequest()
        assertEquals("GET", getRequest.method)
        assertEquals("/v1/isolated/session/sess-old", getRequest.path)

        // delete() must still work by session id.
        session.delete()
        val deleteRequest = mockWebServer.takeRequest()
        assertEquals("DELETE", deleteRequest.method)
        assertEquals("/v1/isolated/session/sess-old", deleteRequest.path)
    }

    @Test
    fun `getInternal populatesFullState whenExecdReturnsAllFields`() {
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "status": "active",
                      "created_at": "2026-01-02T03:04:05Z",
                      "last_run_at": "2026-01-02T03:05:06Z",
                      "idle_remaining_seconds": 30,
                      "profile": "strict",
                      "workspace": {"path": "/workspace", "mode": "rw"},
                      "extra_writable": ["/tmp", "/var/tmp"],
                      "binds": [
                        {"source": "/host/a", "dest": "/sbx/a", "readonly": true}
                      ],
                      "share_net": false,
                      "env_passthrough": {"mode": "allow", "keys": ["PATH", "HOME"]},
                      "uid": 1000,
                      "gid": 2000,
                      "uid_mode": "userns",
                      "idle_timeout_seconds": 300
                    }
                    """.trimIndent(),
                ),
        )

        val state = adapter.getInternal("sess-full")

        val request = mockWebServer.takeRequest()
        assertEquals("GET", request.method)
        assertEquals("/v1/isolated/session/sess-full", request.path)
        assertEquals("route-token", request.getHeader("X-Execd-Token"))

        assertEquals("active", state.status)
        assertEquals(2026, state.createdAt?.year)
        assertEquals(5, state.lastRunAt?.minute)
        assertEquals(30, state.idleRemainingSeconds)
        assertEquals("strict", state.profile)
        assertEquals("/workspace", state.workspace?.path)
        assertEquals("rw", state.workspace?.mode)
        assertEquals(listOf("/tmp", "/var/tmp"), state.extraWritable)
        assertEquals(1, state.binds?.size)
        assertEquals("/host/a", state.binds?.get(0)?.source)
        assertEquals("/sbx/a", state.binds?.get(0)?.dest)
        assertEquals(true, state.binds?.get(0)?.readonly)
        assertEquals(false, state.shareNet)
        assertEquals("allow", state.envPassthrough?.mode)
        assertEquals(listOf("PATH", "HOME"), state.envPassthrough?.keys)
        assertEquals(1000, state.uid)
        assertEquals(2000, state.gid)
        assertEquals("userns", state.uidMode)
        assertEquals(300, state.idleTimeoutSeconds)
    }

    @Test
    fun `getInternal toleratesMissingEchoFields whenExecdIsOlder`() {
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "status": "active",
                      "created_at": "2026-01-02T03:04:05Z",
                      "last_run_at": null,
                      "idle_remaining_seconds": 7
                    }
                    """.trimIndent(),
                ),
        )

        val state = adapter.getInternal("sess-old")

        val request = mockWebServer.takeRequest()
        assertEquals("GET", request.method)
        assertEquals("/v1/isolated/session/sess-old", request.path)

        assertEquals("active", state.status)
        assertNotNull(state.createdAt)
        assertNull(state.lastRunAt)
        assertEquals(7, state.idleRemainingSeconds)
        assertNull(state.profile)
        assertNull(state.workspace)
        assertNull(state.extraWritable)
        assertNull(state.binds)
        assertNull(state.shareNet)
        assertNull(state.envPassthrough)
        assertNull(state.uid)
        assertNull(state.gid)
        assertNull(state.uidMode)
        assertNull(state.idleTimeoutSeconds)
    }

    @Test
    fun `getInternal preservesIdleTimeoutZero`() {
        // idle_timeout_seconds == 0 means "idle GC disabled" (long-window recovery),
        // which is semantically distinct from null/absent. It must be preserved as 0,
        // not coerced to null.
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "status": "active",
                      "created_at": "2026-01-02T03:04:05Z",
                      "idle_timeout_seconds": 0
                    }
                    """.trimIndent(),
                ),
        )

        val state = adapter.getInternal("sess-zero")

        assertEquals("active", state.status)
        assertEquals(0, state.idleTimeoutSeconds)
    }

    @Test
    fun `attach propagatesNotFound whenSessionMissing`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(404)
                .setBody(
                    """
                    {
                      "code": "SESSION_NOT_FOUND",
                      "message": "isolated session not found"
                    }
                    """.trimIndent(),
                ),
        )

        val ex =
            assertThrows(SandboxApiException::class.java) {
                adapter.attach("missing-sess")
            }
        assertEquals(404, ex.statusCode)
        assertTrue(ex.message!!.contains("attach isolated session"))

        val request = mockWebServer.takeRequest()
        assertEquals("GET", request.method)
        assertEquals("/v1/isolated/session/missing-sess", request.path)
    }
}
