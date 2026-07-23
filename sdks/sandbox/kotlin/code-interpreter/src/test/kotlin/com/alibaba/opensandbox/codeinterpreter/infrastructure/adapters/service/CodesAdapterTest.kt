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

package com.alibaba.opensandbox.codeinterpreter.infrastructure.adapters.service

import com.alibaba.opensandbox.codeinterpreter.domain.models.execd.executions.RunCodeRequest
import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionHandlers
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

class CodesAdapterTest {
    private lateinit var mockWebServer: MockWebServer
    private lateinit var codesAdapter: CodesAdapter
    private lateinit var httpClientProvider: HttpClientProvider

    @BeforeEach
    fun setUp() {
        mockWebServer = MockWebServer()
        mockWebServer.start()

        val host = mockWebServer.hostName
        val port = mockWebServer.port
        val config =
            ConnectionConfig.builder()
                .domain("$host:$port")
                .protocol("http")
                .build()

        val endpoint = SandboxEndpoint("$host:$port")
        httpClientProvider = HttpClientProvider(config)
        codesAdapter = CodesAdapter(endpoint, httpClientProvider)
    }

    @AfterEach
    fun tearDown() {
        mockWebServer.shutdown()
        httpClientProvider.close()
    }

    @Test
    fun `createContext should send correct request`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody("""{"id":"ctx-123", "language":"python"}"""),
        )

        val context = codesAdapter.createContext("python")

        assertEquals("ctx-123", context.id)
        assertEquals("python", context.language)

        val request = mockWebServer.takeRequest()
        assertEquals("POST", request.method)
        assertEquals("/code/context", request.path)
    }

    @Test
    fun `createContext should include endpoint headers`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody("""{"id":"ctx-123", "language":"python"}"""),
        )

        val host = mockWebServer.hostName
        val port = mockWebServer.port
        val config =
            ConnectionConfig.builder()
                .domain("$host:$port")
                .protocol("http")
                .build()
        val endpoint = SandboxEndpoint("$host:$port", mapOf("X-Endpoint" to "endpoint"))

        HttpClientProvider(config).use { provider ->
            val adapter = CodesAdapter(endpoint, provider)
            adapter.createContext("python")
        }

        val request = mockWebServer.takeRequest()
        assertEquals("endpoint", request.getHeader("X-Endpoint"))
    }

    @Test
    fun `run should stream events correctly`() {
        // SSE format
        val event1 = """{"type":"stdout","text":"Hello World","timestamp":1672531200000}"""
        val event2 = """{"type":"execution_complete","execution_time":100,"timestamp":1672531201000}"""

        val responseBody = "$event1\n$event2\n"

        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody(responseBody),
        )

        val receivedOutput = StringBuilder()
        val latch = CountDownLatch(1)
        var executionTime = -1L

        val handlers =
            ExecutionHandlers.builder()
                .onStdout { msg -> receivedOutput.append(msg.text) }
                .onExecutionComplete { complete ->
                    executionTime = complete.executionTimeInMillis
                    latch.countDown()
                }
                .build()

        val request =
            RunCodeRequest.builder()
                .code("print('Hello World')")
                .handlers(handlers)
                .build()

        val execution = codesAdapter.run(request)

        assertTrue(latch.await(2, TimeUnit.SECONDS), "Timed out waiting for completion")
        assertEquals("Hello World", receivedOutput.toString())
        assertEquals(100L, executionTime)
        assertEquals(100L, execution.complete?.executionTimeInMillis)
        assertEquals(null, execution.exitCode)

        val recordedRequest = mockWebServer.takeRequest()
        assertEquals("/code", recordedRequest.path)
        assertEquals("POST", recordedRequest.method)
    }

    @Test
    fun `run should include endpoint headers`() {
        val event1 = """{"type":"stdout","text":"Hello World","timestamp":1672531200000}"""
        val event2 = """{"type":"execution_complete","execution_time":100,"timestamp":1672531201000}"""

        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody("$event1\n$event2\n"),
        )

        val host = mockWebServer.hostName
        val port = mockWebServer.port
        val config =
            ConnectionConfig.builder()
                .domain("$host:$port")
                .protocol("http")
                .build()
        val endpoint = SandboxEndpoint("$host:$port", mapOf("X-Endpoint" to "endpoint"))

        HttpClientProvider(config).use { provider ->
            val adapter = CodesAdapter(endpoint, provider)
            val request =
                RunCodeRequest.builder()
                    .code("print('Hello World')")
                    .handlers(ExecutionHandlers.builder().build())
                    .build()

            adapter.run(request)
        }

        val recordedRequest = mockWebServer.takeRequest()
        assertEquals("endpoint", recordedRequest.getHeader("X-Endpoint"))
    }

    @Test
    fun `interrupt should send correct request`() {
        mockWebServer.enqueue(MockResponse().setResponseCode(204))

        codesAdapter.interrupt("exec-123")

        val request = mockWebServer.takeRequest()
        assertEquals("DELETE", request.method)
        assertEquals("/code", request.requestUrl?.encodedPath)
        assertEquals("exec-123", request.requestUrl?.queryParameter("id"))
    }

    @Test
    fun `deleteContexts should send language query`() {
        mockWebServer.enqueue(MockResponse().setResponseCode(204))

        codesAdapter.deleteContexts("python")

        val request = mockWebServer.takeRequest()
        assertEquals("DELETE", request.method)
        assertEquals("/code/contexts", request.requestUrl?.encodedPath)
        assertEquals("python", request.requestUrl?.queryParameter("language"))
    }

    @Test
    fun `run should expose request id on api exception`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(500)
                .addHeader("X-Request-ID", "req-kotlin-code-123")
                .setBody("""{"code":"INTERNAL_ERROR","message":"boom"}"""),
        )

        val request = RunCodeRequest.builder().code("print('boom')").build()
        val ex = assertThrows(SandboxApiException::class.java) { codesAdapter.run(request) }

        assertEquals(500, ex.statusCode)
        assertEquals("req-kotlin-code-123", ex.requestId)
    }
}
