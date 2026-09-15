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
import com.alibaba.opensandbox.sandbox.api.execd.CommandApi
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.ClientException
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.exceptions.InvalidArgumentException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxRateLimitException
import com.alibaba.opensandbox.sandbox.domain.models.execd.SECURE_ACCESS_HEADER
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionHandlers
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionInstance
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunCommandRequest
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunInSessionRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toCommandTimeoutMillis
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.intOrNull
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.mockwebserver.Dispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNotSame
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.assertThrows
import java.time.Duration
import java.util.concurrent.CountDownLatch
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger

class CommandsAdapterTest {
    private fun instanceResponse(issuedAt: Int = 123) =
        MockResponse().setBody(
            """{"instance_id":"scope","issued_at":$issuedAt,"retention_seconds":86400,"capacity":4096}""",
        )

    @Test
    fun `instance cache shares requests and returns independent snapshots until expiry`() {
        val entered = CountDownLatch(1)
        val release = CountDownLatch(1)
        val calls = AtomicInteger()
        mockWebServer.dispatcher =
            object : Dispatcher() {
                override fun dispatch(request: RecordedRequest): MockResponse {
                    val issuedAt = calls.incrementAndGet()
                    entered.countDown()
                    check(release.await(5, TimeUnit.SECONDS))
                    return instanceResponse(issuedAt)
                }
            }
        val pool = Executors.newFixedThreadPool(16)
        try {
            val tasks = (1..16).map { pool.submit<ExecutionInstance> { commandsAdapter.getExecutionInstance() } }
            assertTrue(entered.await(5, TimeUnit.SECONDS))
            release.countDown()
            val values = tasks.map { it.get(5, TimeUnit.SECONDS) }
            assertEquals(1, calls.get())
            values.forEach { assertEquals(1L, it.issuedAt) }
            assertNotSame(values[0], values[1])
            assertNotSame(values[0], commandsAdapter.getExecutionInstance())
            CommandsAdapter::class.java.getDeclaredField("instanceFetchedAt").apply { isAccessible = true }
                .setLong(commandsAdapter, System.nanoTime() - TimeUnit.MINUTES.toNanos(1))
            assertEquals(2L, commandsAdapter.getExecutionInstance().issuedAt)
        } finally {
            release.countDown()
            pool.shutdownNow()
        }
    }

    @Test
    fun `instance fetch failure is not cached`() {
        // Use a terminal HTTP error so the existing transport retry policy is not involved.
        mockWebServer.enqueue(MockResponse().setResponseCode(400).setBody("""{"code":"invalid","message":"bad request"}"""))
        mockWebServer.enqueue(instanceResponse())
        assertThrows<SandboxApiException> { commandsAdapter.getExecutionInstance() }
        assertEquals("scope", commandsAdapter.getExecutionInstance().instanceId)
        assertEquals(2, mockWebServer.requestCount)
    }

    @Test
    fun `operation errors invalidate old inflight instance without replay`() {
        for (code in listOf("operation_instance_mismatch", "operation_expired")) {
            for (method in listOf("command", "pty", "lookup")) {
                val adapter = CommandsAdapter(httpClientProvider, SandboxEndpoint("${mockWebServer.hostName}:${mockWebServer.port}"))
                val entered = CountDownLatch(1)
                val release = CountDownLatch(1)
                val gets = AtomicInteger()
                val operations = AtomicInteger()
                mockWebServer.dispatcher =
                    object : Dispatcher() {
                        override fun dispatch(request: RecordedRequest): MockResponse {
                            if (request.path == "/execution/instance") {
                                val issuedAt = gets.incrementAndGet()
                                if (issuedAt == 1) {
                                    entered.countDown()
                                    check(release.await(5, TimeUnit.SECONDS))
                                }
                                return instanceResponse(issuedAt)
                            }
                            operations.incrementAndGet()
                            if (request.method == "POST") {
                                val body = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
                                assertEquals("saved.identity", body["operation_id"]?.jsonPrimitive?.content)
                            } else {
                                assertEquals("saved.identity", request.getHeader("X-EXECD-OPERATION-ID"))
                            }
                            return MockResponse().setResponseCode(409).setBody("""{"code":"$code","message":"unknown outcome"}""")
                        }
                    }
                val pool = Executors.newSingleThreadExecutor()
                try {
                    val old = pool.submit<ExecutionInstance> { adapter.getExecutionInstance() }
                    assertTrue(entered.await(5, TimeUnit.SECONDS))
                    assertThrows<SandboxApiException> {
                        when (method) {
                            "command" ->
                                adapter.createCommandOperation(
                                    "saved.identity",
                                    RunCommandRequest.builder().command("true").build(),
                                )
                            "pty" -> adapter.createPTYOperation("saved.identity", "", "")
                            else -> adapter.getExecutionOperation("command", "saved.identity")
                        }
                    }
                    assertEquals(2L, adapter.getExecutionInstance().issuedAt)
                    release.countDown()
                    assertEquals(1L, old.get(5, TimeUnit.SECONDS).issuedAt)
                    assertEquals(2L, adapter.getExecutionInstance().issuedAt)
                    assertEquals(2, gets.get())
                    assertEquals(1, operations.get())
                } finally {
                    release.countDown()
                    pool.shutdownNow()
                }
            }
        }
    }

    @Test
    fun `operation creation preserves identity and creation state`() {
        mockWebServer.enqueue(
            MockResponse().setBody("""{"instance_id":"scope","issued_at":123,"retention_seconds":86400,"capacity":4096}"""),
        )
        val instance = commandsAdapter.getExecutionInstance()
        assertTrue(instance.newOperationId().startsWith("scope.123."))
        mockWebServer.takeRequest()
        val response = """{"id":"original","kind":"command","state":"creating","expires_at":"2026-09-09T00:00:00Z"}"""
        mockWebServer.enqueue(MockResponse().setResponseCode(202).setBody(response))
        val operation =
            commandsAdapter.createCommandOperation(
                "scope.123.persisted",
                RunCommandRequest.builder().command("echo hello").build(),
            )
        assertEquals("creating", operation.state)
        val body = Json.parseToJsonElement(mockWebServer.takeRequest().body.readUtf8()).jsonObject
        assertEquals("scope.123.persisted", body["operation_id"]?.jsonPrimitive?.content)
        assertEquals("echo hello", body["command"]?.jsonPrimitive?.content)
        assertTrue(body["argv"] == null)
        mockWebServer.enqueue(MockResponse().setResponseCode(202).setBody(response))
        assertEquals(operation.id, commandsAdapter.getExecutionOperation("command", "scope.123.persisted").id)
        assertEquals("scope.123.persisted", mockWebServer.takeRequest().getHeader("X-EXECD-OPERATION-ID"))
        mockWebServer.enqueue(MockResponse().setResponseCode(202).setBody(response))
        assertEquals(operation.id, commandsAdapter.createPTYOperation("scope.123.persisted", "/tmp", "echo hello").id)
        val ptyBody = Json.parseToJsonElement(mockWebServer.takeRequest().body.readUtf8()).jsonObject
        assertEquals("scope.123.persisted", ptyBody["operation_id"]?.jsonPrimitive?.content)
    }

    // CommandsAdapter unit tests
    @Test
    fun `native operation creation preserves argv`() {
        mockWebServer.enqueue(
            MockResponse().setResponseCode(202)
                .setBody("""{"id":"native","kind":"command","state":"creating","expires_at":"2026-09-09T00:00:00Z"}"""),
        )
        val argv = listOf("/bin/echo", "argument with spaces", "$(literal)")
        commandsAdapter.createCommandOperation("scope.123.persisted", RunCommandRequest.builder().argv(argv).build())
        val body = Json.parseToJsonElement(mockWebServer.takeRequest().body.readUtf8()).jsonObject
        assertEquals(Json.parseToJsonElement("""["/bin/echo","argument with spaces","$(literal)"]"""), body["argv"])
        assertTrue(body["command"] == null)
        assertEquals("scope.123.persisted", body["operation_id"]?.jsonPrimitive?.content)
    }

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
    fun `native argv preserves arguments and omits shell command`() {
        val argv = listOf("tool", "", "a b", "$" + "HOME", "x'y")
        mockWebServer.enqueue(
            MockResponse().setResponseCode(200).setBody("""{"type":"execution_complete","execution_time":1}""" + "\n"),
        )
        commandsAdapter.run(RunCommandRequest.builder().argv(argv).workingDirectory("$" + "DIR").build())
        val body = Json.parseToJsonElement(mockWebServer.takeRequest().body.readUtf8()).jsonObject
        assertTrue("command" !in body)
        assertEquals(argv, body["argv"]?.jsonArray?.map { it.jsonPrimitive.content })
        assertThrows<IllegalArgumentException> { RunCommandRequest.builder().command("echo").argv(argv).build() }
        assertThrows<IllegalArgumentException> { RunCommandRequest.builder().argv(emptyList()).build() }
        assertEquals("$" + "DIR", body["cwd"]?.jsonPrimitive?.content)
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
        assertTrue("argv" !in requestBodyJson)
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
        val responseBody = """{"code":"INTERNAL_ERROR","message":"boom"}"""
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(500)
                .addHeader("X-Request-ID", "req-kotlin-123")
                .setBody(responseBody),
        )

        val request = RunCommandRequest.builder().command("echo Hello").build()
        val ex = assertThrows(SandboxApiException::class.java) { commandsAdapter.run(request) }

        assertEquals(500, ex.statusCode)
        assertEquals("req-kotlin-123", ex.requestId)
        assertEquals(responseBody, ex.responseBody)
    }

    @Test
    fun `run should map unstructured rate limit response metadata`() {
        val responseBody = "slow down"
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(429)
                .addHeader("X-Request-ID", "req-rate-limit")
                .addHeader("Retry-After", "2")
                .setBody(responseBody),
        )

        val request = RunCommandRequest.builder().command("echo Hello").build()
        val ex = assertThrows(SandboxRateLimitException::class.java) { commandsAdapter.run(request) }

        assertEquals(429, ex.statusCode)
        assertEquals("RATE_LIMIT", ex.error.code)
        assertEquals("req-rate-limit", ex.requestId)
        assertEquals(Duration.ofSeconds(2), ex.retryAfter)
        assertEquals(responseBody, ex.responseBody)
        assertTrue(ex.isRetryable)
    }

    @Test
    fun `getBackgroundCommandLogs should include response body in client error`() {
        val responseBody = """{"code":"INVALID_ARGUMENT","message":"cursor must be positive"}"""
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(400)
                .setBody(responseBody),
        )

        val ex =
            assertThrows(SandboxApiException::class.java) {
                commandsAdapter.getBackgroundCommandLogs("exec-1")
            }

        assertEquals(400, ex.statusCode)
        assertTrue(ex.message!!.contains(responseBody))
        assertEquals(responseBody, ex.responseBody)
    }

    @Test
    fun `generated client error message should include response body`() {
        val responseBody = """{"code":"QUOTA_EXCEEDED","message":"sandbox quota exceeded"}"""
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(400)
                .setBody(responseBody),
        )

        val api =
            CommandApi(
                "http://${mockWebServer.hostName}:${mockWebServer.port}",
                httpClientProvider.httpClient,
            )

        val ex = assertThrows(ClientException::class.java) { api.getCommandStatus("exec-1") }

        assertTrue(ex.message!!.contains(responseBody))
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
                    .timeout(Duration.ofSeconds(5))
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
        assertTrue("argv" !in requestBodyJson)
        assertEquals("/workspace", requestBodyJson["cwd"]?.jsonPrimitive?.content)
        assertEquals(5000L, requestBodyJson["timeout"]?.jsonPrimitive?.content?.toLong())
    }

    @Test
    fun `command timeout conversion should reject durations too large for milliseconds`() {
        val exception =
            assertThrows(IllegalArgumentException::class.java) {
                Duration.ofSeconds(Long.MAX_VALUE).toCommandTimeoutMillis()
            }
        assertTrue(exception.message!!.contains("too large to represent in milliseconds"))
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
}
