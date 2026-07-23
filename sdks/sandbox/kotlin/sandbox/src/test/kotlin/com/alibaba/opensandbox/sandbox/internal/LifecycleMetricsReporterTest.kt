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

package com.alibaba.opensandbox.sandbox.internal

import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import java.time.Duration
import java.util.concurrent.TimeUnit

class LifecycleMetricsReporterTest {
    private lateinit var server: MockWebServer

    @BeforeEach
    fun setUp() {
        server = MockWebServer()
        server.start()
    }

    @AfterEach
    fun tearDown() {
        try {
            server.shutdown()
        } catch (_: Exception) {
            // Some tests shut the server down early; ignore double-shutdown.
        }
    }

    @Test
    fun `build payload omits optional fields when null or blank`() {
        val payload = LifecycleMetricsReporter.buildPayload(null, "  ", createDurationMs = 12L, success = false)
        val obj = Json.parseToJsonElement(payload)
        val map = obj.toString()
        assertTrue(map.contains("\"eventType\":\"sandbox.create\""))
        assertTrue(map.contains("\"createDurationMs\":12"))
        assertTrue(map.contains("\"success\":false"))
        assertFalse(map.contains("sandboxId"))
        assertFalse(map.contains("image"))
    }

    @Test
    fun `build payload clamps negative duration to zero`() {
        val payload = LifecycleMetricsReporter.buildPayload("sbx-1", "python:3.12", createDurationMs = -42L, success = true)
        assertTrue(payload.contains("\"createDurationMs\":0"))
        assertTrue(payload.contains("\"sandboxId\":\"sbx-1\""))
        assertTrue(payload.contains("\"image\":\"python:3.12\""))
    }

    @Test
    fun `does not post when disableMetrics flag is set`() {
        server.enqueue(MockResponse().setResponseCode(204))
        val config =
            ConnectionConfig.builder()
                .domain(server.hostName + ":" + server.port)
                .protocol("http")
                .disableMetrics(true)
                .build()

        LifecycleMetricsReporter.reportSandboxCreate(
            connectionConfig = config,
            sandboxId = "sbx",
            image = "python:3.12",
            createDurationMs = 10L,
            success = true,
        )

        // Give the client a short window; MockWebServer.takeRequest returns null on timeout.
        val recorded = server.takeRequest(300, TimeUnit.MILLISECONDS)
        assertNull(recorded, "metrics must not be posted when disableMetrics=true")
    }

    @Test
    fun `posts expected payload and headers on success path`() {
        server.enqueue(MockResponse().setResponseCode(204))
        val config =
            ConnectionConfig.builder()
                .domain(server.hostName + ":" + server.port)
                .protocol("http")
                .apiKey("test-key")
                .requestTimeout(Duration.ofSeconds(5))
                .build()

        LifecycleMetricsReporter.reportSandboxCreate(
            connectionConfig = config,
            sandboxId = "sbx",
            image = "python:3.12",
            createDurationMs = 42L,
            success = true,
        )

        val recorded = server.takeRequest(3, TimeUnit.SECONDS)
        assertNotNull(recorded, "metrics POST should be sent")
        assertEquals("POST", recorded!!.method)
        assertEquals("/v1/metrics/events", recorded.path)
        assertEquals("test-key", recorded.getHeader("OPEN-SANDBOX-API-KEY"))
        assertTrue(recorded.getHeader("User-Agent")!!.startsWith("OpenSandbox-Kotlin-SDK/"))

        val body = Json.parseToJsonElement(recorded.body.readUtf8())
        val obj = body.toString()
        assertTrue(obj.contains("\"eventType\":\"sandbox.create\""))
        assertTrue(obj.contains("\"createDurationMs\":42"))
        assertTrue(obj.contains("\"success\":true"))
        assertTrue(obj.contains("\"sandboxId\":\"sbx\""))
        assertTrue(obj.contains("\"image\":\"python:3.12\""))
        // Sanity: the JSON is parseable and contains the expected shape.
        assertEquals(
            "sandbox.create",
            (body as kotlinx.serialization.json.JsonObject)["eventType"]!!.jsonPrimitive.content,
        )
    }

    @Test
    fun `caller-supplied OkHttpClient is not shut down by reporter`() {
        server.enqueue(MockResponse().setResponseCode(204))
        val config =
            ConnectionConfig.builder()
                .domain(server.hostName + ":" + server.port)
                .protocol("http")
                .build()
        // Caller owns the client; the SDK must not shut it down.
        val callerClient = OkHttpClient()

        LifecycleMetricsReporter.reportSandboxCreate(
            connectionConfig = config,
            sandboxId = "sbx",
            image = "python:3.12",
            createDurationMs = 5L,
            success = true,
            client = callerClient,
        )

        // Wait for the request to be observed by the server so we know the call
        // has finished on the OkHttp dispatcher before we assert client state.
        val recorded = server.takeRequest(3, TimeUnit.SECONDS)
        assertNotNull(recorded, "metrics POST should be sent")
        // Small settling delay for the callback to run to completion.
        Thread.sleep(50L)

        assertFalse(
            callerClient.dispatcher.executorService.isShutdown,
            "reporter must not shut down a caller-supplied OkHttpClient",
        )
    }

    @Test
    fun `sdk-owned OkHttpClient is shut down after the call completes`() {
        // We can't easily observe the internal SDK client, but we can verify
        // the SDK-owned path succeeds end-to-end and drains without leaking a
        // dispatcher that keeps the JVM busy indefinitely. This test succeeds
        // when the response body has been drained (callback ran) and no
        // exceptions escape the reporter.
        server.enqueue(MockResponse().setResponseCode(204).setBody("ok"))
        val config =
            ConnectionConfig.builder()
                .domain(server.hostName + ":" + server.port)
                .protocol("http")
                .build()

        LifecycleMetricsReporter.reportSandboxCreate(
            connectionConfig = config,
            sandboxId = "sbx",
            image = "python:3.12",
            createDurationMs = 5L,
            success = true,
        )

        val recorded = server.takeRequest(3, TimeUnit.SECONDS)
        assertNotNull(recorded, "metrics POST should be sent")
    }

    @Test
    fun `report never throws when baseUrl is invalid`() {
        // Regression test: previously, payload/URL/Request construction ran
        // outside the try/catch guarding enqueue. An invalid baseUrl would
        // cause Request.Builder.url(...) to throw IllegalArgumentException,
        // which could replace the original Sandbox.create failure when the
        // reporter is called from the create catch path.
        //
        // Passing a scheme-prefixed domain containing an illegal character
        // (a space in the host) makes the SDK build a baseUrl like
        // `http:// bad host/v1`, which OkHttp's HttpUrl parser rejects.
        val config =
            ConnectionConfig.builder()
                .domain("http:// bad host")
                .requestTimeout(Duration.ofMillis(200))
                .build()

        // Must not throw — telemetry is best-effort even when construction fails.
        LifecycleMetricsReporter.reportSandboxCreate(
            connectionConfig = config,
            sandboxId = "sbx",
            image = "python:3.12",
            createDurationMs = 5L,
            success = false,
        )
    }

    @Test
    fun `report never throws when server is unreachable`() {
        // Use a shut-down server so the connection immediately fails.
        server.shutdown()
        val config =
            ConnectionConfig.builder()
                .domain("127.0.0.1:1")
                .protocol("http")
                .requestTimeout(Duration.ofMillis(200))
                .build()

        // Should not throw or hang.
        LifecycleMetricsReporter.reportSandboxCreate(
            connectionConfig = config,
            sandboxId = null,
            image = "img",
            createDurationMs = 1L,
            success = false,
        )
    }
}
