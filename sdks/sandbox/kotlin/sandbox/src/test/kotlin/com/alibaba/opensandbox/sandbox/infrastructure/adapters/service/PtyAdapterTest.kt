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
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.assertThrows

class PtyAdapterTest {
    private lateinit var mockWebServer: MockWebServer
    private lateinit var httpClientProvider: HttpClientProvider
    private lateinit var ptyAdapter: PtyAdapter

    @BeforeEach
    fun setUp() {
        mockWebServer = MockWebServer()
        mockWebServer.start()
        val host = mockWebServer.hostName
        val port = mockWebServer.port
        val endpoint = SandboxEndpoint("$host:$port")
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
        mockWebServer.enqueue(MockResponse().setResponseCode(200))

        ptyAdapter.deleteSession("sess-123")

        val recorded = mockWebServer.takeRequest()
        assertEquals("/pty/sess-123", recorded.path)
        assertEquals("DELETE", recorded.method)
    }

    @Test
    fun `createSession should map error responses to SandboxApiException`() {
        mockWebServer.enqueue(MockResponse().setResponseCode(500).setBody("boom"))

        assertThrows<SandboxApiException> {
            ptyAdapter.createSession()
        }
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
    }
}
