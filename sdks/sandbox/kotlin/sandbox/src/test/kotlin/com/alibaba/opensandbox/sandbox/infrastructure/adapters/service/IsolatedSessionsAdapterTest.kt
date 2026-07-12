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
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
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
}
