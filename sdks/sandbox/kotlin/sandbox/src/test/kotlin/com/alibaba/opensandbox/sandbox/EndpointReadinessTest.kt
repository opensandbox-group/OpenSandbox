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

package com.alibaba.opensandbox.sandbox

import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxReadyTimeoutException
import com.alibaba.opensandbox.sandbox.transport.RequestDeadline
import io.opentelemetry.context.Context
import io.opentelemetry.context.ContextKey
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.SocketPolicy
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertSame
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.time.Duration
import java.util.concurrent.atomic.AtomicReference

class EndpointReadinessTest {
    @Test
    fun `health timeout does not report a recovered endpoint error`() {
        val budget = ReadinessBudget(Duration.ofSeconds(1), Duration.ofMillis(1))
        var attempts = 0
        budget.endpoint {
            if (++attempts == 1) throw unavailable()
            "published"
        }

        val caller = Thread.currentThread()
        var healthThread: Thread? = null
        var healthAttempts = 0
        var finished = false
        val error =
            assertThrows(SandboxReadyTimeoutException::class.java) {
                budget.health("test health context") {
                    healthThread = Thread.currentThread()
                    healthAttempts++
                    Thread.sleep(1100)
                    finished = true
                    true
                }
            }
        assertSame(caller, healthThread)
        assertEquals(1, healthAttempts)
        assertTrue(finished)
        assertNull(error.cause)
        assertFalse(error.message!!.contains("starting"))
        assertTrue(error.message!!.contains("health check timed out"))
    }

    @Test
    fun `bounded request preserves caller telemetry context`() {
        val key = ContextKey.named<String>("readiness-test")
        val client = OkHttpClient()
        Context.current().with(key, "caller").makeCurrent().use {
            val observed =
                RequestDeadline.within(System.nanoTime() + Duration.ofSeconds(10).toNanos()) {
                    RequestDeadline.execute(client) { Context.current().get(key) }
                }
            assertEquals("caller", observed)
        }
    }

    private fun unavailable(
        code: String = "KUBERNETES::POD_IP_NOT_AVAILABLE",
        status: Int = 404,
    ) = SandboxApiException("starting", statusCode = status, error = SandboxError(code))

    @Test
    fun `connect and resume wait for egress without refetching execd`() {
        for (resume in listOf(false, true)) {
            MockWebServer().use { server ->
                server.start()
                if (resume) server.enqueue(MockResponse().setResponseCode(204))
                val endpoint = """{"endpoint":"localhost:44772","headers":{"token":"new"}}"""
                server.enqueue(MockResponse().setBody(endpoint))
                repeat(2) {
                    server.enqueue(
                        MockResponse().setResponseCode(404).setBody("""{"code":"KUBERNETES::POD_IP_NOT_AVAILABLE","message":"starting"}"""),
                    )
                }
                server.enqueue(MockResponse().setBody(endpoint))
                val config = ConnectionConfig.builder().domain(server.url("/").toString()).build()
                val sandbox =
                    if (resume) {
                        Sandbox.resumer().sandboxId("sb").connectionConfig(config).skipHealthCheck()
                            .healthCheckPollingInterval(Duration.ofMillis(1)).resume()
                    } else {
                        Sandbox.connector().sandboxId("sb").connectionConfig(config).skipHealthCheck()
                            .healthCheckPollingInterval(Duration.ofMillis(1)).connect()
                    }
                sandbox.close()
                if (resume) assertTrue(server.takeRequest().path!!.endsWith("/resume"))
                assertTrue(server.takeRequest().path!!.contains("/44772"))
                repeat(3) { assertTrue(server.takeRequest().path!!.contains("/18080")) }
                assertEquals(if (resume) 5 else 4, server.requestCount)
            }
        }
    }

    @Test
    fun `permanent errors do not retry and timeout preserves last error`() {
        for (error in listOf(unavailable("SANDBOX_NOT_FOUND"), unavailable(status = 401), unavailable(status = 403))) {
            var calls = 0
            val actual =
                assertThrows(SandboxApiException::class.java) {
                    ReadinessBudget(Duration.ofSeconds(1), Duration.ofMillis(1)).endpoint<Int> {
                        calls++
                        throw error
                    }
                }
            assertSame(error, actual)
            assertEquals(1, calls)
        }
        val error = unavailable()
        val actual =
            assertThrows(SandboxReadyTimeoutException::class.java) {
                ReadinessBudget(Duration.ofMillis(20), Duration.ofMillis(1)).endpoint<Int> { throw error }
            }
        assertSame(error, actual.cause)
    }

    @Test
    fun `in flight endpoint is bounded and interruptible`() {
        for (interrupt in listOf(false, true)) {
            MockWebServer().use { server ->
                server.start()
                server.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
                val failure = AtomicReference<Throwable>()
                val thread =
                    Thread {
                        try {
                            Sandbox.connector().sandboxId("sb")
                                .connectionConfig(ConnectionConfig.builder().domain(server.url("/").toString()).build())
                                .connectTimeout(Duration.ofMillis(300)).skipHealthCheck().connect()
                        } catch (error: Throwable) {
                            failure.set(error)
                        }
                    }
                thread.start()
                assertNotNull(server.takeRequest(2, java.util.concurrent.TimeUnit.SECONDS))
                if (interrupt) thread.interrupt()
                thread.join(1500)
                assertFalse(thread.isAlive)
                if (interrupt) {
                    assertTrue(failure.get() is InterruptedException)
                } else {
                    assertTrue(failure.get() is SandboxReadyTimeoutException)
                }
            }
        }
    }

    @Test
    fun `endpoint time is deducted from health budget`() {
        MockWebServer().use { server ->
            server.start()
            val endpoint = """{"endpoint":"${server.hostName}:${server.port}","headers":{"token":"new"}}"""
            server.enqueue(MockResponse().setBody(endpoint).setHeadersDelay(150, java.util.concurrent.TimeUnit.MILLISECONDS))
            server.enqueue(MockResponse().setBody(endpoint))
            server.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
            val start = System.nanoTime()
            assertThrows(SandboxReadyTimeoutException::class.java) {
                Sandbox.connector().sandboxId("sb")
                    .connectionConfig(ConnectionConfig.builder().domain(server.url("/").toString()).build())
                    .connectTimeout(Duration.ofMillis(300)).connect()
            }
            assertTrue(Duration.ofNanos(System.nanoTime() - start).toMillis() < 430)
            assertEquals(3, server.requestCount)
            server.takeRequest()
            server.takeRequest()
            assertEquals("new", server.takeRequest().getHeader("token"))
        }
    }

    @Test
    fun `transport retry after cannot exceed connection budget`() {
        MockWebServer().use { server ->
            server.start()
            server.enqueue(MockResponse().setResponseCode(503).setHeader("Retry-After", "10"))
            val start = System.nanoTime()
            assertThrows(SandboxReadyTimeoutException::class.java) {
                Sandbox.connector().sandboxId("sb")
                    .connectionConfig(ConnectionConfig.builder().domain(server.url("/").toString()).build())
                    .connectTimeout(Duration.ofMillis(200)).connect()
            }
            assertTrue(Duration.ofNanos(System.nanoTime() - start).toMillis() < 600)
            assertEquals(1, server.requestCount)
        }
    }

    @Test
    fun `custom health can consume a stream before its response completes`() {
        val consumed = java.util.concurrent.CountDownLatch(1)
        val consumedBeforeCompletion = java.util.concurrent.atomic.AtomicBoolean(false)
        val server = com.sun.net.httpserver.HttpServer.create(java.net.InetSocketAddress("127.0.0.1", 0), 0)
        server.createContext("/events") { exchange ->
            exchange.sendResponseHeaders(200, 0)
            exchange.responseBody.use { body ->
                body.write("data: first\n\n".toByteArray())
                body.flush()
                consumedBeforeCompletion.set(consumed.await(2, java.util.concurrent.TimeUnit.SECONDS))
                body.write("data: last\n\n".toByteArray())
            }
        }
        server.start()
        try {
            HttpClientProvider(ConnectionConfig.builder().build()).use { provider ->
                ReadinessBudget(Duration.ofSeconds(5), Duration.ofMillis(1)).health("test") {
                    val request = okhttp3.Request.Builder().url("http://127.0.0.1:${server.address.port}/events").build()
                    provider.sseClient.newCall(request).execute().use { response ->
                        assertEquals("data: first", response.body!!.source().readUtf8Line())
                        consumed.countDown()
                        response.body!!.string()
                    }
                    true
                }
            }
            assertTrue(consumedBeforeCompletion.get(), "health callback must receive events before EOF")
        } finally {
            consumed.countDown()
            server.stop(0)
        }
    }
}
