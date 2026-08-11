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

package com.alibaba.opensandbox.sandbox.transport

import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertSame
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import java.io.IOException
import java.io.InterruptedIOException
import java.time.Duration
import java.util.Random
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger
import javax.net.ssl.SSLException

class RetryInterceptorTest {
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

    private fun client(
        policy: RetryPolicy,
        retryOnConnectionFailure: Boolean = false,
    ): OkHttpClient =
        OkHttpClient.Builder()
            .retryOnConnectionFailure(retryOnConnectionFailure)
            .connectTimeout(2, TimeUnit.SECONDS)
            .readTimeout(2, TimeUnit.SECONDS)
            // Deterministic rng + no-op sleeper so tests do not actually wait.
            .addInterceptor(RetryInterceptor(policy, rng = Random(0), sleeper = {}))
            .build()

    private fun get(client: OkHttpClient): Int {
        val req = Request.Builder().url(server.url("/")).get().build()
        client.newCall(req).execute().use { return it.code }
    }

    private fun post(
        client: OkHttpClient,
        body: String = "x",
    ): Int {
        val req =
            Request.Builder()
                .url(server.url("/"))
                .post(body.toRequestBody())
                .build()
        client.newCall(req).execute().use { return it.code }
    }

    @Test
    fun `GET retries 503 then succeeds`() {
        server.enqueue(MockResponse().setResponseCode(503))
        server.enqueue(MockResponse().setResponseCode(503))
        server.enqueue(MockResponse().setResponseCode(200))

        val code = get(client(RetryPolicy()))
        assertEquals(200, code)
        assertEquals(3, server.requestCount)
    }

    @Test
    fun `GET exhausts retries and returns last 503`() {
        repeat(4) { server.enqueue(MockResponse().setResponseCode(503)) }

        val code = get(client(RetryPolicy()))
        assertEquals(503, code)
        // initial + 3 retries
        assertEquals(4, server.requestCount)
    }

    @Test
    fun `POST does not retry 503 under default policy`() {
        server.enqueue(MockResponse().setResponseCode(503))

        val code = post(client(RetryPolicy()))
        assertEquals(503, code)
        assertEquals(1, server.requestCount)
    }

    @Test
    fun `POST retries 502 when opted in`() {
        server.enqueue(MockResponse().setResponseCode(502))
        server.enqueue(MockResponse().setResponseCode(502))
        server.enqueue(MockResponse().setResponseCode(200))

        val policy = RetryPolicy(retryableStatusCodesNonIdempotent = setOf(429, 502))
        val code = post(client(policy))
        assertEquals(200, code)
        assertEquals(3, server.requestCount)
    }

    @Test
    fun `4xx is never retried`() {
        server.enqueue(MockResponse().setResponseCode(404))
        val code = get(client(RetryPolicy()))
        assertEquals(404, code)
        assertEquals(1, server.requestCount)
    }

    @Test
    fun `Retry-After header drives backoff`() {
        server.enqueue(MockResponse().setResponseCode(429).setHeader("Retry-After", "0"))
        server.enqueue(MockResponse().setResponseCode(200))

        var observedBackoff: Duration? = null
        val policy =
            RetryPolicy(
                onRetry = { observedBackoff = it.backoff },
            )
        val code = get(client(policy))
        assertEquals(200, code)
        assertEquals(2, server.requestCount)
        assertEquals(Duration.ZERO, observedBackoff)
    }

    @Test
    fun `Retry-After is capped at 60s`() {
        server.enqueue(MockResponse().setResponseCode(429).setHeader("Retry-After", "3600"))
        server.enqueue(MockResponse().setResponseCode(200))

        var observedBackoff: Duration? = null
        val policy = RetryPolicy(onRetry = { observedBackoff = it.backoff })
        get(client(policy))
        assertEquals(Duration.ofSeconds(60), observedBackoff)
    }

    @Test
    fun `disabled policy is single attempt on 503`() {
        server.enqueue(MockResponse().setResponseCode(503))
        // disabled() does not wrap, but if used explicitly it must not retry.
        val code = get(client(RetryPolicy(maxRetries = 0, onRetry = {})))
        assertEquals(503, code)
        assertEquals(1, server.requestCount)
    }

    @Test
    fun `pre-send connect failure retries POST`() {
        // Point at a port with no listener to force a ConnectException (pre-send).
        val deadPort = server.port
        server.shutdown()
        val req =
            Request.Builder()
                .url("http://127.0.0.1:$deadPort/")
                .post("x".toRequestBody())
                .build()
        var attempts = 0
        val policy = RetryPolicy(initialBackoff = Duration.ofMillis(1), onRetry = { attempts++ })
        val c =
            OkHttpClient.Builder()
                .retryOnConnectionFailure(false)
                .connectTimeout(500, TimeUnit.MILLISECONDS)
                .addInterceptor(RetryInterceptor(policy, rng = Random(0), sleeper = {}))
                .build()
        assertThrows(IOException::class.java) { c.newCall(req).execute() }
        // initial + 3 retries => 3 onRetry events
        assertEquals(3, attempts)
    }

    @Test
    fun `GET retries opaque SSL failure`() {
        server.enqueue(MockResponse().setResponseCode(200))
        val attempts = AtomicInteger()
        val c =
            OkHttpClient.Builder()
                .retryOnConnectionFailure(false)
                .addInterceptor(RetryInterceptor(RetryPolicy(), rng = Random(0), sleeper = {}))
                .addInterceptor { chain ->
                    if (attempts.incrementAndGet() == 1) {
                        throw SSLException("mid-stream TLS failure")
                    }
                    chain.proceed(chain.request())
                }
                .build()

        assertEquals(200, get(c))
        assertEquals(2, attempts.get())
        assertEquals(1, server.requestCount)
    }

    @Test
    fun `POST does not retry opaque SSL failure`() {
        val attempts = AtomicInteger()
        val c =
            OkHttpClient.Builder()
                .retryOnConnectionFailure(false)
                .addInterceptor(RetryInterceptor(RetryPolicy(), rng = Random(0), sleeper = {}))
                .addInterceptor { chain ->
                    attempts.incrementAndGet()
                    throw SSLException("mid-stream TLS failure")
                }
                .build()

        val req =
            Request.Builder()
                .url(server.url("/"))
                .post("x".toRequestBody())
                .build()
        assertThrows(SSLException::class.java) { c.newCall(req).execute() }
        assertEquals(1, attempts.get())
        assertEquals(0, server.requestCount)
    }

    @Test
    fun `backoff interruption preserves thread interrupt flag`() {
        val original = IOException("connection reset")
        val attempts = AtomicInteger()
        val c =
            OkHttpClient.Builder()
                .retryOnConnectionFailure(false)
                .addInterceptor(
                    RetryInterceptor(
                        RetryPolicy(maxRetries = 1),
                        rng = Random(0),
                        sleeper = { Thread.currentThread().interrupt() },
                    ),
                )
                .addInterceptor {
                    attempts.incrementAndGet()
                    throw original
                }
                .build()

        // Avoid inheriting a stale interrupt from another test, then clean it up
        // after asserting so this test does not affect the JUnit worker thread.
        Thread.interrupted()
        try {
            val thrown = assertThrows(IOException::class.java) { get(c) }
            assertSame(original, thrown)
            assertTrue(Thread.currentThread().isInterrupted)
            assertEquals(1, attempts.get())
        } finally {
            Thread.interrupted()
        }
    }

    @Test
    fun `IO interruption is terminal and preserves thread interrupt flag`() {
        val original = InterruptedIOException("interrupted")
        val attempts = AtomicInteger()
        val retries = AtomicInteger()
        val c =
            OkHttpClient.Builder()
                .retryOnConnectionFailure(false)
                .addInterceptor(
                    RetryInterceptor(
                        RetryPolicy(onRetry = { retries.incrementAndGet() }),
                        rng = Random(0),
                        sleeper = {},
                    ),
                )
                .addInterceptor {
                    attempts.incrementAndGet()
                    Thread.currentThread().interrupt()
                    throw original
                }
                .build()

        Thread.interrupted()
        try {
            val thrown = assertThrows(InterruptedIOException::class.java) { get(c) }
            assertSame(original, thrown)
            assertTrue(Thread.currentThread().isInterrupted)
            assertEquals(1, attempts.get())
            assertEquals(0, retries.get())
        } finally {
            Thread.interrupted()
        }
    }

    @Test
    fun `overallDeadline enables wrapsTransport`() {
        // The interceptor deadline path is tested at the decision level in
        // RetryDecisionTest. Here we verify the wiring: a deadline-installed
        // interceptor still passes through normal success/failure responses
        // without flaky clock dependencies.
        val policy = RetryPolicy(maxRetries = 1, overallDeadline = Duration.ofSeconds(10))
        assertTrue(policy.wrapsTransport())
        server.enqueue(MockResponse().setResponseCode(503))
        server.enqueue(MockResponse().setResponseCode(200))
        val client =
            OkHttpClient.Builder()
                .retryOnConnectionFailure(false)
                .addInterceptor(RetryInterceptor(policy, rng = Random(0), sleeper = {}))
                .build()
        val code = get(client)
        assertEquals(200, code)
        assertEquals(2, server.requestCount)
    }

    @Test
    fun `onRetry receives request id`() {
        server.enqueue(MockResponse().setResponseCode(503).setHeader("X-Request-ID", "req-1"))
        server.enqueue(MockResponse().setResponseCode(200))

        var event: RetryEvent? = null
        val policy = RetryPolicy(onRetry = { event = it })
        get(client(policy))
        assertEquals("req-1", event?.requestId)
        assertEquals(503, event?.statusCode)
        assertEquals(2, event?.attempt)
    }
}
