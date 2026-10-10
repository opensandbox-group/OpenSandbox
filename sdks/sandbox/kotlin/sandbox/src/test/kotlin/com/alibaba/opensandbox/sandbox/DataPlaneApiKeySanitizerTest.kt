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

/**
 * Data-plane clients (httpClient, sseClient) must never carry a tenant API
 * key into the sandbox in direct mode: execd performs no authentication, so
 * a key set via the connection's custom headers would be readable by
 * untrusted sandbox code. Server-proxy requests pass the server's auth gate
 * and keep the key.
 */
class DataPlaneApiKeySanitizerTest {
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

    private fun execute(
        client: okhttp3.OkHttpClient,
        path: String = "/",
    ) {
        server.enqueue(MockResponse().setResponseCode(200))
        val request = Request.Builder().url(server.url(path)).build()
        client.newCall(request).execute().use { it.body?.close() }
    }

    @Test
    fun `httpClient strips manually set API key in direct mode`() {
        val config =
            ConnectionConfig.builder()
                .addHeader("OPEN-SANDBOX-API-KEY", "tenant-secret")
                .useServerProxy(false)
                .build()

        HttpClientProvider(config).use { provider ->
            execute(provider.httpClient)
        }

        assertNull(server.takeRequest().getHeader("OPEN-SANDBOX-API-KEY"))
    }

    @Test
    fun `httpClient keeps manually set API key when server proxy declared`() {
        val config =
            ConnectionConfig.builder()
                .addHeader("OPEN-SANDBOX-API-KEY", "tenant-secret")
                .useServerProxy(true)
                .build()

        HttpClientProvider(config).use { provider ->
            execute(provider.httpClient)
        }

        assertEquals("tenant-secret", server.takeRequest().getHeader("OPEN-SANDBOX-API-KEY"))
    }

    @Test
    fun `sseClient strips manually set API key in direct mode`() {
        val config =
            ConnectionConfig.builder()
                .addHeader("open-sandbox-api-key", "tenant-secret")
                .useServerProxy(false)
                .build()

        HttpClientProvider(config).use { provider ->
            execute(provider.sseClient)
        }

        assertNull(server.takeRequest().getHeader("OPEN-SANDBOX-API-KEY"))
    }

    @Test
    fun `sseClient keeps manually set API key when server proxy declared`() {
        val config =
            ConnectionConfig.builder()
                .addHeader("open-sandbox-api-key", "tenant-secret")
                .useServerProxy(true)
                .build()

        HttpClientProvider(config).use { provider ->
            execute(provider.sseClient)
        }

        assertEquals("tenant-secret", server.takeRequest().getHeader("OPEN-SANDBOX-API-KEY"))
    }

    @Test
    fun `custom headers other than the API key are untouched in direct mode`() {
        val config =
            ConnectionConfig.builder()
                .addHeader("X-Request-ID", "trace-123")
                .useServerProxy(false)
                .build()

        HttpClientProvider(config).use { provider ->
            execute(provider.httpClient)
        }

        assertEquals("trace-123", server.takeRequest().getHeader("X-Request-ID"))
    }

    @Test
    fun `key re-added by an interceptor appended after client cloning is still stripped in direct mode`() {
        val config =
            ConnectionConfig.builder()
                .useServerProxy(false)
                .build()

        HttpClientProvider(config).use { provider ->
            // Adapters clone the shared client and append interceptors that
            // inject endpoint headers; the sanitizer runs at the network
            // layer, after all application interceptors.
            val cloned =
                provider.httpClient.newBuilder()
                    .addInterceptor { chain ->
                        chain.proceed(
                            chain.request().newBuilder()
                                .header("OPEN-SANDBOX-API-KEY", "endpoint-key")
                                .build(),
                        )
                    }
                    .build()
            execute(cloned)
        }

        assertNull(server.takeRequest().getHeader("OPEN-SANDBOX-API-KEY"))
    }
}
