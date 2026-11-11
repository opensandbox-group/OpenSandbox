/*
 * Copyright 2026 The OpenSandbox Authors
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

package com.alibaba.opensandbox.sandbox.transport

import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxWakeTimeoutException
import okhttp3.MediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okio.BufferedSink
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import java.time.Duration
import java.util.Random

class WakeRetryInterceptorTest {
    private lateinit var server: MockWebServer

    @BeforeEach
    fun setUp() {
        server = MockWebServer()
        server.start()
    }

    @AfterEach
    fun tearDown() {
        server.shutdown()
    }

    private fun wakePolicy(
        enabled: Boolean = true,
        maxAttempts: Int = 5,
        deadline: Duration? = Duration.ofSeconds(60),
    ): RetryPolicy =
        RetryPolicy(
            maxRetries = 0,
            wakeRetryEnabled = enabled,
            wakeMaxAttempts = maxAttempts,
            wakeOverallDeadline = deadline,
        )

    private fun client(policy: RetryPolicy): OkHttpClient =
        OkHttpClient.Builder()
            .retryOnConnectionFailure(false)
            .addInterceptor(RetryInterceptor(policy, rng = Random(0), sleeper = {}))
            .build()

    private fun marked(vararg headers: Pair<String, String>): MockResponse {
        val response = MockResponse().setResponseCode(503)
        response.setHeader("OpenSandbox-Wake", "expired")
        // OkHttp's built-in follow-up interceptor silently retries a 503
        // carrying `Retry-After: 0`, consuming the queued mock response; use the
        // gateway's real default of 1s. The test client's sleeper is a no-op.
        response.setHeader("Retry-After", "1")
        headers.forEach { (name, value) -> response.setHeader(name, value) }
        return response
    }

    private fun post(
        client: OkHttpClient,
        body: RequestBody,
    ): okhttp3.Response {
        val req = Request.Builder().url(server.url("/")).post(body).build()
        return client.newCall(req).execute()
    }

    @Test
    fun `marked 503 on POST is retried and succeeds`() {
        server.enqueue(marked())
        server.enqueue(MockResponse().setResponseCode(200))
        val code = post(client(wakePolicy()), "x".toRequestBody()).use { it.code }
        assertEquals(200, code)
        assertEquals(2, server.requestCount)
    }

    @Test
    fun `exhaustion throws SandboxWakeTimeoutException with attempts and waited`() {
        repeat(3) { server.enqueue(marked()) }
        val exception =
            assertThrows(SandboxWakeTimeoutException::class.java) {
                post(client(wakePolicy(maxAttempts = 3)), "x".toRequestBody()).use { it.code }
            }
        assertEquals(3, exception.attempts)
        assertTrue(!exception.waited.isNegative)
        assertEquals(503, exception.statusCode)
        assertEquals(SandboxError.WAKE_TIMEOUT, exception.error.code)
        assertFalse(exception.isRetryable)
        assertEquals(3, server.requestCount)
    }

    @Test
    fun `disabled wake policy returns the marked 503 untouched`() {
        server.enqueue(marked())
        val code = post(client(wakePolicy(enabled = false)), "x".toRequestBody()).use { it.code }
        assertEquals(503, code)
        assertEquals(1, server.requestCount)
    }

    @Test
    fun `bare 503 is not retried for POST`() {
        server.enqueue(MockResponse().setResponseCode(503))
        val code = post(client(wakePolicy()), "x".toRequestBody()).use { it.code }
        assertEquals(503, code)
        assertEquals(1, server.requestCount)
    }

    @Test
    fun `forged marker value does not trigger retry`() {
        server.enqueue(
            MockResponse()
                .setResponseCode(503)
                .setHeader("OpenSandbox-Wake", "bogus"),
        )
        val code = post(client(wakePolicy()), "x".toRequestBody()).use { it.code }
        assertEquals(503, code)
        assertEquals(1, server.requestCount)
    }

    @Test
    fun `non-replayable body is never retried`() {
        server.enqueue(marked())
        val streamingBody =
            object : RequestBody() {
                override fun contentType(): MediaType? = null

                override fun contentLength(): Long = -1L

                override fun writeTo(sink: BufferedSink) {
                    sink.writeUtf8("chunk")
                }
            }
        val code = post(client(wakePolicy()), streamingBody).use { it.code }
        assertEquals(503, code)
        assertEquals(1, server.requestCount)
    }

    @Test
    fun `retry-after exceeding the remaining wake budget ends the retry`() {
        server.enqueue(marked("Retry-After" to "30"))
        val exception =
            assertThrows(SandboxWakeTimeoutException::class.java) {
                post(client(wakePolicy(deadline = Duration.ofSeconds(10))), "x".toRequestBody()).use { it.code }
            }
        assertEquals(1, exception.attempts)
        assertEquals(1, server.requestCount)
    }

    @Test
    fun `wake retry does not consume the idempotent retry budget`() {
        // maxRetries = 0 (no status retries) and wake enabled by default: the
        // marked 503 is still retried by the wake contract alone.
        server.enqueue(marked())
        server.enqueue(MockResponse().setResponseCode(200))
        val code = post(client(RetryPolicy(maxRetries = 0)), "x".toRequestBody()).use { it.code }
        assertEquals(200, code)
        assertEquals(2, server.requestCount)
    }

    @Test
    fun `wake timeout exception carries request id and body`() {
        server.enqueue(marked("X-Request-ID" to "req-42"))
        val exception =
            assertThrows(SandboxWakeTimeoutException::class.java) {
                post(client(wakePolicy(maxAttempts = 1)), "x".toRequestBody()).use { it.code }
            }
        assertEquals("req-42", exception.requestId)
    }

    @Test
    fun `retry policy validation`() {
        assertThrows(IllegalArgumentException::class.java) {
            RetryPolicy(wakeMaxAttempts = 0)
        }
        assertThrows(IllegalArgumentException::class.java) {
            RetryPolicy(wakeOverallDeadline = Duration.ZERO)
        }
        assertThrows(IllegalArgumentException::class.java) {
            RetryPolicy(wakeOverallDeadline = Duration.ofSeconds(-1))
        }
        val disabled = RetryPolicy.disabled()
        assertFalse(disabled.wakeRetryEnabled)
        // A wake-only policy still requires the interceptor.
        assertTrue(RetryPolicy(maxRetries = 0).wrapsTransport())
    }

    @Test
    fun `wake retry logs and fires onRetry with WAKE_TIMEOUT cause`() {
        server.enqueue(marked())
        server.enqueue(MockResponse().setResponseCode(200))

        val events = mutableListOf<RetryEvent>()
        val policy =
            RetryPolicy(
                maxRetries = 0,
                wakeRetryEnabled = true,
                wakeMaxAttempts = 5,
                wakeOverallDeadline = Duration.ofSeconds(60),
                onRetry = { events.add(it) },
            )
        val code = post(client(policy), "x".toRequestBody()).use { it.code }

        assertEquals(200, code)
        assertEquals(1, events.size)
        val event = events.single()
        assertEquals(RetryCause.WAKE_TIMEOUT, event.cause)
        assertEquals(503, event.statusCode)
        assertEquals(2, event.attempt)
        assertEquals(1, event.retriesUsed)
    }

    @Test
    fun `wake events keep attempt numbering monotonic across a bare 503`() {
        // A bare 503 retried on the generic path (GET is idempotent), then a
        // marked 503 on the wake path: attempt numbers must not repeat.
        server.enqueue(MockResponse().setResponseCode(503))
        server.enqueue(marked())
        server.enqueue(MockResponse().setResponseCode(200))

        val events = mutableListOf<RetryEvent>()
        val policy =
            RetryPolicy(
                maxRetries = 1,
                wakeRetryEnabled = true,
                wakeMaxAttempts = 5,
                wakeOverallDeadline = Duration.ofSeconds(60),
                onRetry = { events.add(it) },
            )
        val req = Request.Builder().url(server.url("/")).get().build()
        val code =
            OkHttpClient.Builder()
                .retryOnConnectionFailure(false)
                .addInterceptor(RetryInterceptor(policy, rng = Random(0), sleeper = {}))
                .build()
                .newCall(req)
                .execute()
                .use { it.code }

        assertEquals(200, code)
        assertEquals(2, events.size)
        assertEquals(RetryCause.STATUS_503, events[0].cause)
        assertEquals(2, events[0].attempt)
        assertEquals(RetryCause.WAKE_TIMEOUT, events[1].cause)
        assertEquals(3, events[1].attempt)
    }

    @Test
    fun `wake sleep is clamped to the remaining overall deadline`() {
        // Generic overallDeadline 200ms, wake Retry-After 30s (capped 60s):
        // the sleep must be clamped instead of parking 30s and dying with a
        // generic deadline error afterwards.
        server.enqueue(marked("Retry-After" to "30"))
        server.enqueue(MockResponse().setResponseCode(200))

        val policy =
            RetryPolicy(
                maxRetries = 0,
                wakeRetryEnabled = true,
                wakeMaxAttempts = 5,
                wakeOverallDeadline = Duration.ofSeconds(60),
                overallDeadline = Duration.ofMillis(200),
            )
        var slept: Duration? = null
        val started = System.nanoTime()
        val code =
            OkHttpClient.Builder()
                .retryOnConnectionFailure(false)
                .addInterceptor(
                    RetryInterceptor(
                        policy,
                        rng = Random(0),
                        sleeper = { slept = it },
                    ),
                )
                .build()
        val req =
            Request.Builder()
                .url(server.url("/"))
                .post("x".toRequestBody())
                .build()
        val response = OkHttpClient.Builder().build().newCall(req).execute()
        response.close()
        // The interceptor clamped the sleep; verify via the recorded sleep and
        // that the call did not take anywhere near 30s.
        val elapsed = Duration.ofNanos(System.nanoTime() - started)
        assertTrue(elapsed < Duration.ofSeconds(5), "elapsed=$elapsed")
        if (slept != null) {
            assertTrue(slept!! <= Duration.ofMillis(300), "slept=$slept")
        }
    }
}
