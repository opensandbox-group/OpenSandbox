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

package com.alibaba.opensandbox.sandbox

import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import okhttp3.Request
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

class ApiKeyHeaderTest {
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

    private fun getAuthenticated(config: ConnectionConfig) {
        HttpClientProvider(config).use { provider ->
            val request = Request.Builder().url(server.url("/")).build()
            provider.authenticatedClient.newCall(request).execute().use { it.body?.close() }
        }
    }

    @Test
    fun `authenticated request carries the configured api key`() {
        server.enqueue(MockResponse().setResponseCode(200))

        getAuthenticated(
            ConnectionConfig.builder()
                .apiKey("configured-key")
                .build(),
        )

        val recorded = server.takeRequest()
        assertEquals(listOf("configured-key"), recorded.headers.values("OPEN-SANDBOX-API-KEY"))
    }

    @Test
    fun `user-provided api key header is not overwritten`() {
        server.enqueue(MockResponse().setResponseCode(200))

        getAuthenticated(
            ConnectionConfig.builder()
                .apiKey("configured-key")
                .addHeader("OPEN-SANDBOX-API-KEY", "explicit-key")
                .build(),
        )

        val recorded = server.takeRequest()
        assertEquals(listOf("explicit-key"), recorded.headers.values("OPEN-SANDBOX-API-KEY"))
    }

    @Test
    fun `user-provided api key header in lowercase is not overwritten`() {
        server.enqueue(MockResponse().setResponseCode(200))

        getAuthenticated(
            ConnectionConfig.builder()
                .apiKey("configured-key")
                .addHeader("open-sandbox-api-key", "explicit-key")
                .build(),
        )

        val recorded = server.takeRequest()
        assertEquals(listOf("explicit-key"), recorded.headers.values("OPEN-SANDBOX-API-KEY"))
    }

    @Test
    fun `api key header is omitted when no key is configured`() {
        server.enqueue(MockResponse().setResponseCode(200))

        getAuthenticated(
            ConnectionConfig.builder()
                .headers(mapOf("X-Custom" to "value"))
                .build(),
        )

        val recorded = server.takeRequest()
        assertNull(recorded.getHeader("OPEN-SANDBOX-API-KEY"))
    }
}
