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
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

class DiagnosticsAdapterTest {
    private lateinit var mockWebServer: MockWebServer
    private lateinit var diagnosticsAdapter: DiagnosticsAdapter
    private lateinit var httpClientProvider: HttpClientProvider

    @BeforeEach
    fun setUp() {
        mockWebServer = MockWebServer()
        mockWebServer.start()

        val config =
            ConnectionConfig.builder()
                .domain("${mockWebServer.hostName}:${mockWebServer.port}")
                .protocol("http")
                .apiKey("test-api-key")
                .build()

        httpClientProvider = HttpClientProvider(config)
        diagnosticsAdapter = DiagnosticsAdapter(httpClientProvider)
    }

    @AfterEach
    fun tearDown() {
        mockWebServer.shutdown()
        httpClientProvider.close()
    }

    @Test
    fun `getLogs should send scoped request and parse response`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody(
                    """
                    {
                      "sandboxId": "sbx-1",
                      "kind": "logs",
                      "scope": "container",
                      "delivery": "inline",
                      "contentType": "text/plain; charset=utf-8",
                      "content": "log line",
                      "truncated": false,
                      "warnings": ["partial"]
                    }
                    """.trimIndent(),
                ),
        )

        val result = diagnosticsAdapter.getLogs("sbx-1", "container")

        val request = mockWebServer.takeRequest()
        assertEquals("GET", request.method)
        assertEquals("/v1/sandboxes/sbx-1/diagnostics/logs?scope=container", request.path)
        assertEquals("test-api-key", request.getHeader("OPEN-SANDBOX-API-KEY"))
        assertEquals("application/json", request.getHeader("Accept"))
        assertEquals("logs", result.kind)
        assertEquals("container", result.scope)
        assertEquals("inline", result.delivery)
        assertEquals("log line", result.content)
        assertEquals(listOf("partial"), result.warnings)
    }

    @Test
    fun `getEvents should send scoped request and parse response`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody(
                    """
                    {
                      "sandboxId": "sbx-1",
                      "kind": "events",
                      "scope": "runtime",
                      "delivery": "url",
                      "contentType": "text/plain; charset=utf-8",
                      "contentUrl": "https://example.com/events.txt",
                      "contentLength": 12,
                      "expiresAt": "2026-04-14T10:30:00Z",
                      "truncated": false
                    }
                    """.trimIndent(),
                ),
        )

        val result = diagnosticsAdapter.getEvents("sbx-1", "runtime")

        val request = mockWebServer.takeRequest()
        assertEquals("GET", request.method)
        assertEquals("/v1/sandboxes/sbx-1/diagnostics/events?scope=runtime", request.path)
        assertEquals("application/json", request.getHeader("Accept"))
        assertEquals("events", result.kind)
        assertEquals("runtime", result.scope)
        assertEquals("url", result.delivery)
        assertEquals("https://example.com/events.txt", result.contentUrl.toString())
        assertEquals(12, result.contentLength)
    }
}
