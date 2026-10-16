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
 * Protected OpenSandbox headers (tenant API key, endpoint-scoped
 * credentials) must not be replayed to another origin when a data-plane
 * request is redirected: a 3xx from the endpoint would otherwise deliver
 * the credentials to whatever host the redirect points at. Same-origin
 * redirects keep headers unchanged; unrelated custom headers are never
 * touched. Matches the Python SDK's redirect behavior and #2173 (JS).
 */
class CrossOriginHeaderGuardTest {
    private lateinit var endpoint: MockWebServer
    private lateinit var otherOrigin: MockWebServer

    @BeforeEach
    fun setUp() {
        endpoint = MockWebServer()
        endpoint.start()
        otherOrigin = MockWebServer()
        otherOrigin.start()
    }

    @AfterEach
    fun tearDown() {
        endpoint.shutdown()
        otherOrigin.shutdown()
    }

    /** Mirrors how adapters clone the shared client: endpoint headers are
     *  injected by an application interceptor, the origin guard runs at the
     *  network layer. */
    private fun guardedClient(
        provider: HttpClientProvider,
        baseUrl: String,
    ): okhttp3.OkHttpClient =
        provider.httpClient.newBuilder()
            .addInterceptor { chain ->
                chain.proceed(
                    chain.request().newBuilder()
                        .header("OpenSandbox-Secure-Access", "endpoint-token")
                        .header("X-Request-ID", "trace-1")
                        .build(),
                )
            }
            .addProtectedHeaderOriginGuard(baseUrl)
            .build()

    private fun execute(
        client: okhttp3.OkHttpClient,
        url: okhttp3.HttpUrl,
    ) {
        client.newCall(Request.Builder().url(url).build()).execute().use { it.body?.close() }
    }

    @Test
    fun `cross-origin redirect strips protected headers but keeps unrelated ones`() {
        endpoint.enqueue(
            MockResponse()
                .setResponseCode(302)
                .setHeader("Location", otherOrigin.url("/landing")),
        )
        otherOrigin.enqueue(MockResponse().setResponseCode(200))

        val config =
            ConnectionConfig.builder()
                .apiKey("tenant-secret")
                .useServerProxy(true)
                .build()

        HttpClientProvider(config).use { provider ->
            execute(guardedClient(provider, endpoint.url("/").toString()), endpoint.url("/probe"))
        }

        val hop = otherOrigin.takeRequest()
        assertNull(hop.getHeader("OPEN-SANDBOX-API-KEY"))
        assertNull(hop.getHeader("OpenSandbox-Secure-Access"))
        assertEquals("trace-1", hop.getHeader("X-Request-ID"))
    }

    @Test
    fun `same-origin redirect keeps protected headers`() {
        endpoint.enqueue(
            MockResponse()
                .setResponseCode(302)
                .setHeader("Location", endpoint.url("/landing")),
        )
        endpoint.enqueue(MockResponse().setResponseCode(200))

        val config =
            ConnectionConfig.builder()
                .apiKey("tenant-secret")
                .useServerProxy(true)
                .build()

        HttpClientProvider(config).use { provider ->
            execute(guardedClient(provider, endpoint.url("/").toString()), endpoint.url("/probe"))
        }

        endpoint.takeRequest() // the original probe
        val redirected = endpoint.takeRequest()
        assertEquals("tenant-secret", redirected.getHeader("OPEN-SANDBOX-API-KEY"))
        assertEquals("endpoint-token", redirected.getHeader("OpenSandbox-Secure-Access"))
    }

    @Test
    fun `endpoint-scoped headers are stripped cross-origin in direct mode`() {
        endpoint.enqueue(
            MockResponse()
                .setResponseCode(302)
                .setHeader("Location", otherOrigin.url("/landing")),
        )
        otherOrigin.enqueue(MockResponse().setResponseCode(200))

        val config =
            ConnectionConfig.builder()
                .addHeader("OPEN-SANDBOX-API-KEY", "tenant-secret")
                .useServerProxy(false)
                .build()

        HttpClientProvider(config).use { provider ->
            execute(guardedClient(provider, endpoint.url("/").toString()), endpoint.url("/probe"))
        }

        val hop = otherOrigin.takeRequest()
        assertNull(hop.getHeader("OPEN-SANDBOX-API-KEY"))
        assertNull(hop.getHeader("OpenSandbox-Secure-Access"))
        assertEquals("trace-1", hop.getHeader("X-Request-ID"))
    }
}
