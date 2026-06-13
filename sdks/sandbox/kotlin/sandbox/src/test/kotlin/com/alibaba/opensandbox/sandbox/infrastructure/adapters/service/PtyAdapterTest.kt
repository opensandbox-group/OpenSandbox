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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.service

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.models.execd.pty.PtyMode
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

class PtyAdapterTest {
    private lateinit var mockWebServer: MockWebServer
    private lateinit var httpClientProvider: HttpClientProvider
    private lateinit var ptyAdapter: PtyAdapter
    private lateinit var endpoint: SandboxEndpoint

    @BeforeEach
    fun setUp() {
        mockWebServer = MockWebServer()
        mockWebServer.start()
        val host = mockWebServer.hostName
        val port = mockWebServer.port
        endpoint = SandboxEndpoint("$host:$port")
        val config =
            ConnectionConfig.builder()
                .domain("$host:$port")
                .protocol("http")
                .build()
        httpClientProvider = HttpClientProvider(config)
        ptyAdapter = PtyAdapter(httpClientProvider, endpoint)
    }

    @AfterEach
    fun tearDown() {
        mockWebServer.shutdown()
    }

    @Test
    fun `createSession should POST to pty and parse the session id`() {
        mockWebServer.enqueue(
            MockResponse().setResponseCode(201).setBody("""{"session_id":"sess-123"}"""),
        )

        val session = ptyAdapter.createSession(cwd = "/tmp", command = "bash")

        assertEquals("sess-123", session.sessionId)
        val recorded = mockWebServer.takeRequest()
        assertEquals("/pty", recorded.path)
        assertEquals("POST", recorded.method)
        val body = Json.parseToJsonElement(recorded.body.readUtf8()).jsonObject
        assertEquals("/tmp", body["cwd"]?.jsonPrimitive?.content)
        assertEquals("bash", body["command"]?.jsonPrimitive?.content)
    }

    @Test
    fun `getSession should parse running and output offset`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody("""{"session_id":"sess-123","running":true,"output_offset":4096}"""),
        )

        val status = ptyAdapter.getSession("sess-123")

        assertEquals("sess-123", status.sessionId)
        assertTrue(status.running)
        assertEquals(4096L, status.outputOffset)
        val recorded = mockWebServer.takeRequest()
        assertEquals("/pty/sess-123", recorded.path)
        assertEquals("GET", recorded.method)
    }

    @Test
    fun `deleteSession should issue a DELETE`() {
        mockWebServer.enqueue(MockResponse().setResponseCode(204))

        ptyAdapter.deleteSession("sess-123")

        val recorded = mockWebServer.takeRequest()
        assertEquals("/pty/sess-123", recorded.path)
        assertEquals("DELETE", recorded.method)
    }

    @Test
    fun `createSession should throw SandboxApiException on error status`() {
        mockWebServer.enqueue(MockResponse().setResponseCode(500).setBody("boom"))

        val ex =
            assertThrows(SandboxApiException::class.java) {
                ptyAdapter.createSession()
            }
        assertEquals(500, ex.statusCode)
    }

    @Test
    fun `webSocket should build a ws url with mode since and takeover params`() {
        val base = ptyAdapter.webSocket("sess-123")
        assertEquals("ws://${endpoint.endpoint}/pty/sess-123/ws", base.url)

        val full =
            ptyAdapter.webSocket(
                "sess-123",
                mode = PtyMode.PIPE,
                since = 4096,
                takeover = true,
            )
        assertEquals("ws://${endpoint.endpoint}/pty/sess-123/ws?pty=0&since=4096&takeover=1", full.url)
    }

    @Test
    fun `webSocket should use wss when protocol is https`() {
        val config =
            ConnectionConfig.builder()
                .domain(endpoint.endpoint)
                .protocol("https")
                .build()
        val secureAdapter = PtyAdapter(HttpClientProvider(config), endpoint)

        val target = secureAdapter.webSocket("sess-123")

        assertTrue(target.url.startsWith("wss://"), "Expected wss scheme, got: ${target.url}")
    }

    @Test
    fun `webSocket should carry the endpoint routing and auth headers`() {
        val headers = mapOf("OpenSandbox-Ingress-To" to "sandbox-1-44772", "X-Auth" to "token")
        val adapter = PtyAdapter(httpClientProvider, SandboxEndpoint(endpoint.endpoint, headers))

        val target = adapter.webSocket("sess-123")

        assertEquals(headers, target.headers)
    }

    @Test
    fun `endpoint headers should be forwarded to execd`() {
        val host = mockWebServer.hostName
        val port = mockWebServer.port
        val endpointWithHeaders = SandboxEndpoint("$host:$port", mapOf("X-Test-Header" to "value-1"))
        val adapter = PtyAdapter(httpClientProvider, endpointWithHeaders)
        mockWebServer.enqueue(MockResponse().setResponseCode(201).setBody("""{"session_id":"sess-1"}"""))

        adapter.createSession()

        val recorded = mockWebServer.takeRequest()
        assertEquals("value-1", recorded.getHeader("X-Test-Header"))
        assertFalse(recorded.getHeader("X-Test-Header").isNullOrEmpty())
    }
}
