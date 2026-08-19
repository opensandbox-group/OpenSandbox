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
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedCapabilities
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedRunOpts
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedWorkspaceSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.boolean
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.long
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
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
    fun `capabilities maps uid mode availability`() {
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "available": true,
                      "isolator": "bwrap",
                      "setpriv_available": false,
                      "userns_available": true,
                      "commit_supported": false,
                      "diff_supported": false
                    }
                    """.trimIndent(),
                ),
        )

        val capabilities = adapter.capabilities()

        assertEquals("/v1/isolated/capabilities", mockWebServer.takeRequest().path)
        assertEquals(false, capabilities.setprivAvailable)
        assertEquals(true, capabilities.usernsAvailable)
    }

    @Test
    fun `capabilities preserves existing positional constructor order`() {
        val capabilities = IsolatedCapabilities(true, null, null, null, true, true)

        assertEquals(true, capabilities.commitSupported)
        assertEquals(true, capabilities.diffSupported)
        assertEquals(false, capabilities.setprivAvailable)
        assertEquals(false, capabilities.usernsAvailable)
    }

    @Test
    fun `capabilities parses hardening status`() {
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "available": true,
                      "hardening": {
                        "init_mode": "pid1",
                        "signal_shield": true,
                        "cap_drop": {"state": "active"},
                        "seccomp": {"state": "active"},
                        "landlock": {"state": "unsupported", "message": "kernel ABI < 1"},
                        "ebpf": {"state": "disabled"}
                      }
                    }
                    """.trimIndent(),
                ),
        )
        val capabilities = adapter.capabilities()

        val hardening = capabilities.hardening
        assertEquals("pid1", hardening?.initMode)
        assertEquals(true, hardening?.signalShield)
        assertEquals("active", hardening?.capDrop?.state)
        assertEquals("active", hardening?.seccomp?.state)
        assertEquals("unsupported", hardening?.landlock?.state)
        assertEquals("kernel ABI < 1", hardening?.landlock?.message)
        assertEquals("disabled", hardening?.ebpf?.state)
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

    @Test
    fun `runBackground posts background flag without timeout and parses 202 handle`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(202)
                .setBody(
                    """
                    {
                      "session_id": "sess-1",
                      "run_id": "run-1",
                      "started_at": "2026-01-02T03:04:05Z"
                    }
                    """.trimIndent(),
                ),
        )

        val run =
            adapter.runBackgroundInternal(
                "sess-1",
                "echo hi",
                IsolatedRunOpts(envs = mapOf("A" to "b"), timeoutSeconds = 30),
            )

        val request = mockWebServer.takeRequest()
        assertEquals("POST", request.method)
        assertEquals("/v1/isolated/session/sess-1/run", request.path)
        assertEquals("route-token", request.getHeader("X-Execd-Token"))
        val body = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
        assertEquals("echo hi", body["code"]!!.jsonPrimitive.content)
        assertEquals("b", body["envs"]!!.jsonObject["A"]!!.jsonPrimitive.content)
        assertEquals(true, body["background"]!!.jsonPrimitive.boolean)
        assertNull(body["timeout_seconds"])

        assertEquals("sess-1", run.sessionId)
        assertEquals("run-1", run.runId)
        assertNotNull(run.startedAt)
        assertEquals(2026, run.startedAt?.year)
    }

    @Test
    fun `runBackground defaults opts to empty`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(202)
                .setBody(
                    """
                    {
                      "session_id": "sess-1",
                      "run_id": "run-1"
                    }
                    """.trimIndent(),
                ),
        )

        val run = adapter.runBackgroundInternal("sess-1", "echo hi")

        val request = mockWebServer.takeRequest()
        val body = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
        assertEquals(true, body["background"]!!.jsonPrimitive.boolean)
        assertNull(body["envs"])
        assertNull(body["timeout_seconds"])
        assertEquals("run-1", run.runId)
        assertNull(run.startedAt)
    }

    @Test
    fun `runBackground propagates http error`() {
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
                adapter.runBackgroundInternal("missing-sess", "echo hi")
            }
        assertEquals(404, ex.statusCode)
        assertTrue(ex.message!!.contains("run background in isolated session"))
        assertEquals("/v1/isolated/session/missing-sess/run", mockWebServer.takeRequest().path)
    }

    @Test
    fun `runBackground validates blank code`() {
        assertThrows(IllegalArgumentException::class.java) {
            adapter.runBackgroundInternal("sess-1", "   ")
        }
        assertThrows(IllegalArgumentException::class.java) {
            adapter.runBackgroundInternal("", "echo hi")
        }
    }

    @Test
    fun `getRunStatus parses running and finished statuses`() {
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "session_id": "sess-1",
                      "run_id": "run-1",
                      "running": true,
                      "started_at": "2026-01-02T03:04:05Z"
                    }
                    """.trimIndent(),
                ),
        )
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "session_id": "sess-1",
                      "run_id": "run-1",
                      "running": false,
                      "exit_code": 7,
                      "error": "session terminated",
                      "started_at": "2026-01-02T03:04:05Z",
                      "finished_at": "2026-01-02T03:04:09Z"
                    }
                    """.trimIndent(),
                ),
        )

        val running = adapter.getRunStatusInternal("sess-1", "run-1")
        val finished = adapter.getRunStatusInternal("sess-1", "run-1")

        assertEquals("GET", mockWebServer.takeRequest().method)
        assertEquals("/v1/isolated/session/sess-1/runs/run-1", mockWebServer.takeRequest().path)

        assertTrue(running.running)
        assertNull(running.exitCode)
        assertNull(running.error)
        assertNotNull(running.startedAt)
        assertNull(running.finishedAt)

        assertFalse(finished.running)
        assertEquals(7, finished.exitCode)
        assertEquals("session terminated", finished.error)
        assertNotNull(finished.finishedAt)
        assertEquals(9, finished.finishedAt?.second)
    }

    @Test
    fun `getRunStatus propagates http error`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(404)
                .setBody("""{"code": "RUN_NOT_FOUND"}"""),
        )

        val ex =
            assertThrows(SandboxApiException::class.java) {
                adapter.getRunStatusInternal("sess-1", "run-missing")
            }
        assertEquals(404, ex.statusCode)
        assertEquals("/v1/isolated/session/sess-1/runs/run-missing", mockWebServer.takeRequest().path)
    }

    @Test
    fun `getRunStatus validates blank runId`() {
        assertThrows(IllegalArgumentException::class.java) {
            adapter.getRunStatusInternal("sess-1", "")
        }
    }

    @Test
    fun `getRunLogs sends cursor param and uses header cursor`() {
        mockWebServer.enqueue(
            MockResponse()
                .setBody("line1\nline2\n")
                .addHeader("EXECD-ISOLATED-TAIL-CURSOR", "12"),
        )

        val logs = adapter.getRunLogsInternal("sess-1", "run-1", cursor = 4)

        val request = mockWebServer.takeRequest()
        assertEquals("GET", request.method)
        assertEquals("/v1/isolated/session/sess-1/runs/run-1/logs", request.requestUrl?.encodedPath)
        assertEquals("4", request.requestUrl?.queryParameter("cursor"))

        assertEquals("line1\nline2\n", logs.text)
        assertEquals(12L, logs.cursor)
    }

    @Test
    fun `getRunLogs omits cursor param at zero`() {
        mockWebServer.enqueue(
            MockResponse()
                .setBody("hello")
                .addHeader("EXECD-ISOLATED-TAIL-CURSOR", "5"),
        )

        val logs = adapter.getRunLogsInternal("sess-1", "run-1")

        assertNull(mockWebServer.takeRequest().requestUrl?.queryParameter("cursor"))
        assertEquals("hello", logs.text)
        assertEquals(5L, logs.cursor)
    }

    @Test
    fun `getRunLogs falls back to cursor plus bytes when header absent`() {
        mockWebServer.enqueue(MockResponse().setBody("hello"))

        val logs = adapter.getRunLogsInternal("sess-1", "run-1", cursor = 0)

        assertEquals("hello", logs.text)
        assertEquals(5L, logs.cursor)
    }

    @Test
    fun `getRunLogs falls back when header cursor unparseable`() {
        mockWebServer.enqueue(
            MockResponse()
                .setBody("hello")
                .addHeader("EXECD-ISOLATED-TAIL-CURSOR", "not-a-number"),
        )

        val logs = adapter.getRunLogsInternal("sess-1", "run-1", cursor = 10)

        assertEquals("hello", logs.text)
        assertEquals(15L, logs.cursor)
    }

    @Test
    fun `getRunLogs counts bytes not characters`() {
        // 6 UTF-8 bytes for the two CJK characters.
        mockWebServer.enqueue(MockResponse().setBody("你好"))

        val logs = adapter.getRunLogsInternal("sess-1", "run-1", cursor = 2)

        assertEquals("你好", logs.text)
        assertEquals(8L, logs.cursor)
    }

    @Test
    fun `getRunLogs propagates http error`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(404)
                .setBody("""{"code": "RUN_NOT_FOUND"}"""),
        )

        val ex =
            assertThrows(SandboxApiException::class.java) {
                adapter.getRunLogsInternal("sess-1", "run-missing")
            }
        assertEquals(404, ex.statusCode)
        assertEquals(
            "/v1/isolated/session/sess-1/runs/run-missing/logs",
            mockWebServer.takeRequest().path,
        )
    }

    @Test
    fun `getRunLogs validates negative cursor`() {
        assertThrows(IllegalArgumentException::class.java) {
            adapter.getRunLogsInternal("sess-1", "run-1", cursor = -1)
        }
    }

    @Test
    fun `background run full lifecycle with incremental logs and exit code`() {
        // create session
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "session_id": "sess-1",
                      "created_at": "2026-01-02T03:04:05Z"
                    }
                    """.trimIndent(),
                ),
        )
        // run background
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(202)
                .setBody(
                    """
                    {
                      "session_id": "sess-1",
                      "run_id": "run-1",
                      "started_at": "2026-01-02T03:04:05Z"
                    }
                    """.trimIndent(),
                ),
        )
        // status: still running
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "session_id": "sess-1",
                      "run_id": "run-1",
                      "running": true,
                      "started_at": "2026-01-02T03:04:05Z"
                    }
                    """.trimIndent(),
                ),
        )
        // status: finished with exit code 0
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "session_id": "sess-1",
                      "run_id": "run-1",
                      "running": false,
                      "exit_code": 0,
                      "started_at": "2026-01-02T03:04:05Z",
                      "finished_at": "2026-01-02T03:04:07Z"
                    }
                    """.trimIndent(),
                ),
        )
        // logs page 1
        mockWebServer.enqueue(
            MockResponse()
                .setBody("first line\n")
                .addHeader("EXECD-ISOLATED-TAIL-CURSOR", "11"),
        )
        // logs page 2
        mockWebServer.enqueue(
            MockResponse()
                .setBody("second line\n")
                .addHeader("EXECD-ISOLATED-TAIL-CURSOR", "23"),
        )

        val session =
            adapter.create(
                CreateIsolatedSessionRequest(
                    workspace = IsolatedWorkspaceSpec(path = "/workspace"),
                ),
            )
        assertEquals("/v1/isolated/session", mockWebServer.takeRequest().path)

        val run = session.runBackground("echo background")
        val runRequest = mockWebServer.takeRequest()
        assertEquals("POST", runRequest.method)
        assertEquals("/v1/isolated/session/sess-1/run", runRequest.path)
        val runBody = Json.parseToJsonElement(runRequest.body.readUtf8()).jsonObject
        assertEquals("echo background", runBody["code"]!!.jsonPrimitive.content)
        assertEquals(true, runBody["background"]!!.jsonPrimitive.boolean)
        assertEquals("run-1", run.runId)

        val running = session.getRunStatus(run.runId)
        assertEquals("/v1/isolated/session/sess-1/runs/run-1", mockWebServer.takeRequest().path)
        assertTrue(running.running)

        val finished = session.getRunStatus(run.runId)
        assertEquals("/v1/isolated/session/sess-1/runs/run-1", mockWebServer.takeRequest().path)
        assertFalse(finished.running)
        assertEquals(0, finished.exitCode)
        assertNotNull(finished.finishedAt)

        val page1 = session.getRunLogs(run.runId)
        val page1Request = mockWebServer.takeRequest()
        assertEquals("/v1/isolated/session/sess-1/runs/run-1/logs", page1Request.requestUrl?.encodedPath)
        assertNull(page1Request.requestUrl?.queryParameter("cursor"))
        assertEquals("first line\n", page1.text)
        assertEquals(11L, page1.cursor)

        val page2 = session.getRunLogs(run.runId, cursor = page1.cursor)
        assertEquals("second line\n", page2.text)
        assertEquals(23L, page2.cursor)
        assertEquals("11", mockWebServer.takeRequest().requestUrl?.queryParameter("cursor"))
    }
}
