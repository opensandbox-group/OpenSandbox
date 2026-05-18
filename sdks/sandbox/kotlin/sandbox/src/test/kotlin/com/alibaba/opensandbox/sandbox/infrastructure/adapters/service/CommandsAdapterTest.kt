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
import com.alibaba.opensandbox.sandbox.domain.exceptions.InvalidArgumentException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.models.execd.SECURE_ACCESS_HEADER
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionHandlers
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunCommandRequest
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunInSessionRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.intOrNull
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.SocketPolicy
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.assertThrows
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import kotlin.time.Duration.Companion.seconds

class CommandsAdapterTest {
    // CommandsAdapter unit tests
    private lateinit var mockWebServer: MockWebServer
    private lateinit var commandsAdapter: CommandsAdapter
    private lateinit var httpClientProvider: HttpClientProvider

    @BeforeEach
    fun setUp() {
        mockWebServer = MockWebServer()
        mockWebServer.start()

        // We need to parse the port from MockWebServer to simulate the Execd endpoint
        val host = mockWebServer.hostName
        val port = mockWebServer.port
        val endpoint = SandboxEndpoint("$host:$port")

        val config =
            ConnectionConfig.builder()
                .domain("$host:$port")
                .protocol("http")
                .build()

        httpClientProvider = HttpClientProvider(config)
        commandsAdapter = CommandsAdapter(httpClientProvider, endpoint)
    }

    @AfterEach
    fun tearDown() {
        mockWebServer.shutdown()
        httpClientProvider.close()
    }

    @Test
    fun `run should stream events correctly`() {
        // SSE format: event nodes are JSON objects separated by newlines
        val event1 = """{"type":"stdout","text":"Hello","timestamp":1672531200000}"""
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
            RunCommandRequest.builder()
                .command("echo Hello")
                .uid(1000)
                .gid(1000)
                .env("APP_ENV", "test")
                .env("LOG_LEVEL", "debug")
                .handlers(handlers)
                .build()

        val execution = commandsAdapter.run(request)

        assertTrue(latch.await(2, TimeUnit.SECONDS), "Timed out waiting for completion event")
        assertEquals("Hello", receivedOutput.toString())
        assertEquals(100L, executionTime)
        assertEquals(0, execution.exitCode)
        assertEquals(100L, execution.complete?.executionTimeInMillis)

        val recordedRequest = mockWebServer.takeRequest()
        assertEquals("/command", recordedRequest.path)
        assertEquals("POST", recordedRequest.method)
        val requestBodyJson = Json.parseToJsonElement(recordedRequest.body.readUtf8()).jsonObject
        assertEquals("echo Hello", requestBodyJson["command"]?.jsonPrimitive?.content)
        assertEquals(1000, requestBodyJson["uid"]?.jsonPrimitive?.intOrNull)
        assertEquals(1000, requestBodyJson["gid"]?.jsonPrimitive?.intOrNull)
        val envs = requestBodyJson["envs"]?.jsonObject
        assertEquals("test", envs?.get("APP_ENV")?.jsonPrimitive?.content)
        assertEquals("debug", envs?.get("LOG_LEVEL")?.jsonPrimitive?.content)
        // Builder defaults background to false; request body always includes it
        assertEquals(false, requestBodyJson["background"]?.jsonPrimitive?.booleanOrNull)
    }

    @Test
    fun `endpoint headers should be sent to streaming and generated api requests`() {
        val host = mockWebServer.hostName
        val port = mockWebServer.port
        val endpointProvider =
            HttpClientProvider(
                ConnectionConfig.builder()
                    .domain("$host:$port")
                    .protocol("http")
                    .build(),
            )
        try {
            val adapter =
                CommandsAdapter(
                    endpointProvider,
                    SandboxEndpoint(
                        "$host:$port",
                        mapOf(
                            SECURE_ACCESS_HEADER to "secure-token",
                            "OpenSandbox-Ingress-To" to "sandbox-44772",
                        ),
                    ),
                )

            val completeEvent = """{"type":"execution_complete","execution_time":1,"timestamp":1672531200000}"""
            mockWebServer.enqueue(
                MockResponse()
                    .setResponseCode(200)
                    .setBody("$completeEvent\n"),
            )

            adapter.run(RunCommandRequest.builder().command("echo secure").build())

            val runRequest = mockWebServer.takeRequest()
            assertEquals("secure-token", runRequest.getHeader(SECURE_ACCESS_HEADER))
            assertEquals("sandbox-44772", runRequest.getHeader("OpenSandbox-Ingress-To"))

            mockWebServer.enqueue(
                MockResponse()
                    .setResponseCode(200)
                    .setBody("""{"session_id":"sess-secure"}"""),
            )

            adapter.createSession("/workspace")

            val sessionRequest = mockWebServer.takeRequest()
            assertEquals("secure-token", sessionRequest.getHeader(SECURE_ACCESS_HEADER))
            assertEquals("sandbox-44772", sessionRequest.getHeader("OpenSandbox-Ingress-To"))
        } finally {
            endpointProvider.close()
        }
    }

    @Test
    fun `run should infer non-zero exit code from command error event`() {
        val initEvent = """{"type":"init","text":"cmd-123","timestamp":1672531200000}"""
        val errorEvent =
            """{"type":"error","error":{"ename":"CommandExecError",""" +
                """"evalue":"7","traceback":["exit status 7"]},"timestamp":1672531201000}"""

        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody("$initEvent\n$errorEvent\n"),
        )

        val execution =
            commandsAdapter.run(
                RunCommandRequest.builder()
                    .command("exit 7")
                    .build(),
            )

        assertEquals("cmd-123", execution.id)
        assertEquals(7, execution.exitCode)
        assertEquals("CommandExecError", execution.error?.name)
        assertEquals("7", execution.error?.value)
        assertEquals(null, execution.complete)
    }

    @Test
    fun `run should infer exit code from final execution state regardless of event order`() {
        val initEvent = """{"type":"init","text":"cmd-123","timestamp":1672531200000}"""
        val completeEvent = """{"type":"execution_complete","execution_time":100,"timestamp":1672531201000}"""
        val errorEvent =
            """{"type":"error","error":{"ename":"CommandExecError","evalue":"7",""" +
                """"traceback":["exit status 7"]},"timestamp":1672531202000}"""

        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody("$initEvent\n$completeEvent\n$errorEvent\n"),
        )

        val execution =
            commandsAdapter.run(
                RunCommandRequest.builder()
                    .command("exit 7")
                    .build(),
            )

        assertEquals(7, execution.exitCode)
        assertEquals(100L, execution.complete?.executionTimeInMillis)
        assertEquals("7", execution.error?.value)
    }

    @Test
    fun `run should keep exit code null when command error value is blank`() {
        val initEvent = """{"type":"init","text":"cmd-123","timestamp":1672531200000}"""
        val completeEvent = """{"type":"execution_complete","execution_time":100,"timestamp":1672531201000}"""
        val errorEvent =
            """{"type":"error","error":{"ename":"CommandExecError",""" +
                """"evalue":"","traceback":["failed"]},"timestamp":1672531202000}"""

        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody("$initEvent\n$completeEvent\n$errorEvent\n"),
        )

        val execution =
            commandsAdapter.run(
                RunCommandRequest.builder()
                    .command("bad command")
                    .build(),
            )

        assertEquals(null, execution.exitCode)
        assertEquals("", execution.error?.value)
        assertEquals(100L, execution.complete?.executionTimeInMillis)
    }

    @Test
    fun `run command builder should require uid when gid is provided`() {
        assertThrows<IllegalArgumentException> {
            RunCommandRequest.builder()
                .command("id")
                .gid(1000)
                .build()
        }
    }

    @Test
    fun `run should expose request id on api exception`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(500)
                .addHeader("X-Request-ID", "req-kotlin-123")
                .setBody("""{"code":"INTERNAL_ERROR","message":"boom"}"""),
        )

        val request = RunCommandRequest.builder().command("echo Hello").build()
        val ex = assertThrows(SandboxApiException::class.java) { commandsAdapter.run(request) }

        assertEquals(500, ex.statusCode)
        assertEquals("req-kotlin-123", ex.requestId)
    }

    @Test
    fun `createSession should use generated api and return session id`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody("""{"session_id":"sess-123"}"""),
        )

        val sessionId = commandsAdapter.createSession("/workspace")

        assertEquals("sess-123", sessionId)
        val recordedRequest = mockWebServer.takeRequest()
        assertEquals("/session", recordedRequest.path)
        assertEquals("POST", recordedRequest.method)
        val requestBodyJson = Json.parseToJsonElement(recordedRequest.body.readUtf8()).jsonObject
        assertEquals("/workspace", requestBodyJson["cwd"]?.jsonPrimitive?.content)
    }

    @Test
    fun `runInSession should stream events and send session request payload`() {
        val stdoutEvent = """event: stdout
data: {"type":"stdout","text":"Hello","timestamp":1672531200000}"""
        val completeEvent = """event: execution_complete
data: {"type":"execution_complete","execution_time":100,"timestamp":1672531201000}"""
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody("$stdoutEvent\n\n$completeEvent\n\n"),
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

        val execution =
            commandsAdapter.runInSession(
                "sess-123",
                RunInSessionRequest.builder()
                    .command("echo Hello")
                    .workingDirectory("/workspace")
                    .timeout(5.seconds)
                    .handlers(handlers)
                    .build(),
            )

        assertTrue(latch.await(2, TimeUnit.SECONDS), "Timed out waiting for session completion event")
        assertEquals("Hello", receivedOutput.toString())
        assertEquals(100L, executionTime)
        assertEquals(0, execution.exitCode)
        assertEquals(100L, execution.complete?.executionTimeInMillis)
        val recordedRequest = mockWebServer.takeRequest()
        assertEquals("/session/sess-123/run", recordedRequest.path)
        assertEquals("POST", recordedRequest.method)
        val requestBodyJson = Json.parseToJsonElement(recordedRequest.body.readUtf8()).jsonObject
        assertEquals("echo Hello", requestBodyJson["command"]?.jsonPrimitive?.content)
        assertEquals("/workspace", requestBodyJson["cwd"]?.jsonPrimitive?.content)
        assertEquals(5000L, requestBodyJson["timeout"]?.jsonPrimitive?.content?.toLong())
    }

    @Test
    fun `runInSession should infer non-zero exit code from command error event`() {
        val initEvent = """data: {"type":"init","text":"cmd-123","timestamp":1672531200000}"""
        val errorEvent =
            """data: {"type":"error","error":{"ename":"CommandExecError","evalue":"7",""" +
                """"traceback":["exit status 7"]},"timestamp":1672531201000}"""

        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody("$initEvent\n\n$errorEvent\n\n"),
        )

        val execution =
            commandsAdapter.runInSession(
                "sess-123",
                RunInSessionRequest.builder()
                    .command("exit 7")
                    .build(),
            )

        assertEquals("cmd-123", execution.id)
        assertEquals(7, execution.exitCode)
        assertEquals("CommandExecError", execution.error?.name)
        assertEquals("7", execution.error?.value)
        assertEquals(null, execution.complete)
    }

    @Test
    fun `deleteSession should use generated api`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(204),
        )

        commandsAdapter.deleteSession("sess-123")

        val recordedRequest = mockWebServer.takeRequest()
        assertEquals("/session/sess-123", recordedRequest.path)
        assertEquals("DELETE", recordedRequest.method)
    }

    @Test
    fun `createSession should reject blank workingDirectory`() {
        val ex = assertThrows(InvalidArgumentException::class.java) { commandsAdapter.createSession("   ") }
        assertEquals("workingDirectory cannot be blank when provided", ex.message)
    }

    @Test
    fun `deleteSession should reject blank session id`() {
        val ex = assertThrows(InvalidArgumentException::class.java) { commandsAdapter.deleteSession(" ") }
        assertEquals("session_id cannot be empty", ex.message)
    }

    // -----------------------------------------------------------------------
    // SSE resume test
    // -----------------------------------------------------------------------

    @Test
    fun `run should auto-resume on SSE disconnect`() {
        val totalLines = 40
        val allEvents = buildString {
            appendLine("""{"type":"init","text":"cmd-resume","timestamp":1672531200000,"eid":1}""")
            for (i in 1..totalLines) {
                appendLine("""{"type":"stdout","text":"line$i","timestamp":1672531200000,"eid":${i + 1}}""")
            }
            appendLine("""{"type":"execution_complete","execution_time":100,"timestamp":1672531201000,"eid":${totalLines + 2}}""")
        }

        var resumeCalled = false
        var lastAfterEid: Long? = null
        var postRequestCount = 0

        mockWebServer.dispatcher = object : okhttp3.mockwebserver.Dispatcher() {
            override fun dispatch(request: okhttp3.mockwebserver.RecordedRequest): MockResponse {
                return when {
                    request.method == "POST" && request.path == "/command" -> {
                        postRequestCount++
                        MockResponse()
                            .setResponseCode(200)
                            .setBody(allEvents)
                            .setSocketPolicy(SocketPolicy.DISCONNECT_DURING_RESPONSE_BODY)
                    }
                    request.method == "GET" && request.path!!.contains("/resume") -> {
                        resumeCalled = true
                        lastAfterEid = request.requestUrl!!.queryParameter("after_eid")!!.toLong()
                        val startEid = lastAfterEid!! + 1
                        val remaining = buildString {
                            for (eid in startEid..(totalLines + 1)) {
                                appendLine("""{"type":"stdout","text":"line${eid - 1}","timestamp":1672531200000,"eid":$eid}""")
                            }
                            appendLine(
                                """{"type":"execution_complete","execution_time":100,"timestamp":1672531201000,"eid":${totalLines + 2}}""",
                            )
                        }
                        MockResponse()
                            .setResponseCode(200)
                            .setBody(remaining)
                    }
                    else -> MockResponse().setResponseCode(404)
                }
            }
        }

        val receivedLines = mutableSetOf<String>()
        val latch = CountDownLatch(1)
        val handlers = ExecutionHandlers.builder()
            .onStdout { msg -> receivedLines.add(msg.text) }
            .onExecutionComplete { latch.countDown() }
            .build()

        val request = RunCommandRequest.builder()
            .command("for i in 1..40; do echo line\$i; done")
            .handlers(handlers)
            .build()

        val execution = commandsAdapter.run(request)

        assertTrue(resumeCalled, "Expected resume to be called after disconnect")
        assertTrue(latch.await(5, TimeUnit.SECONDS), "Timed out waiting for completion")
        assertEquals(totalLines, receivedLines.size,
            "Expected all $totalLines lines, got ${receivedLines.size}: $receivedLines")
        for (i in 1..totalLines) {
            assertTrue(receivedLines.contains("line$i"), "Missing line$i")
        }
        assertEquals("cmd-resume", execution.id)
        assertEquals(0, execution.exitCode)
        assertNotNull(execution.complete)
        assertEquals(1, postRequestCount, "Should send exactly one POST /command")
        assertNotNull(lastAfterEid)
        assertTrue(lastAfterEid!! >= 1, "after_eid should be >= 1, got $lastAfterEid")
        assertTrue(execution.lastEid >= totalLines + 1)
    }
}
