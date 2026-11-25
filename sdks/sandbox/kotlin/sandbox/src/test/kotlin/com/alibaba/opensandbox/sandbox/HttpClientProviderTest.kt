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
import com.alibaba.opensandbox.sandbox.domain.models.execd.EXECD_ACCESS_TOKEN_HEADER
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.infrastructure.factory.AdapterFactory
import com.alibaba.opensandbox.sandbox.transport.RetryInterceptor
import com.alibaba.opensandbox.sandbox.transport.RetryPolicy
import okhttp3.Request
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertSame
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.time.Duration

class HttpClientProviderTest {
    @Test
    fun `SSE client disables all automatic retries for every retry policy`() {
        val policies =
            listOf(
                RetryPolicy(),
                RetryPolicy.disabled(),
                RetryPolicy(maxRetries = 0, overallDeadline = Duration.ofSeconds(5)),
            )

        policies.forEach { policy ->
            HttpClientProvider(
                ConnectionConfig.builder()
                    .retryPolicy(policy)
                    .build(),
            ).use { provider ->
                assertFalse(provider.sseClient.retryOnConnectionFailure)
                assertFalse(provider.sseClient.interceptors.any { it is RetryInterceptor })
            }
        }
    }

    @Test
    fun `SSE client bounds the gap between stream bytes but not the whole stream`() {
        mapOf(
            Duration.ofSeconds(5) to 30_000,
            Duration.ofSeconds(120) to 120_000,
        ).forEach { (requestTimeout, readTimeoutMillis) ->
            HttpClientProvider(
                ConnectionConfig.builder()
                    .requestTimeout(requestTimeout)
                    .build(),
            ).use { provider ->
                assertEquals(readTimeoutMillis, provider.sseClient.readTimeoutMillis)
                assertEquals(0, provider.sseClient.callTimeoutMillis)
            }
        }
    }

    @Test
    fun `staged warmup health client is single attempt and shares transport resources`() {
        HttpClientProvider(ConnectionConfig.builder().build().copyForStagedWarmup()).use { provider ->
            assertTrue(provider.httpClient.interceptors.any { it is RetryInterceptor })
            assertFalse(provider.singleAttemptClient.retryOnConnectionFailure)
            assertFalse(provider.singleAttemptClient.interceptors.any { it is RetryInterceptor })
            assertSame(provider.httpClient.dispatcher, provider.singleAttemptClient.dispatcher)
            assertSame(provider.httpClient.connectionPool, provider.singleAttemptClient.connectionPool)
        }
    }

    @Test
    fun `single attempt config disables policy retry and OkHttp recovery without changing public disabled policy`() {
        val retrying =
            ConnectionConfig.builder()
                .retryPolicy(RetryPolicy(retryableStatusCodesNonIdempotent = setOf(429)))
                .build()
        HttpClientProvider(retrying).use { provider ->
            assertFalse(provider.authenticatedClient.retryOnConnectionFailure)
            assertTrue(provider.authenticatedClient.interceptors.any { it is RetryInterceptor })
        }
        HttpClientProvider(retrying.copyForSingleAttempt()).use { provider ->
            assertFalse(provider.authenticatedClient.retryOnConnectionFailure)
            assertFalse(provider.authenticatedClient.interceptors.any { it is RetryInterceptor })
        }

        val publicDisabled =
            ConnectionConfig.builder()
                .retryPolicy(RetryPolicy.disabled())
                .build()

        HttpClientProvider(publicDisabled).use { provider ->
            assertTrue(provider.authenticatedClient.retryOnConnectionFailure)
            assertFalse(provider.authenticatedClient.interceptors.any { it is RetryInterceptor })
        }
        HttpClientProvider(publicDisabled.copyForSingleAttempt()).use { provider ->
            assertFalse(provider.authenticatedClient.retryOnConnectionFailure)
            assertFalse(provider.authenticatedClient.interceptors.any { it is RetryInterceptor })
        }
    }

    @Test
    fun `debug logging interceptor redacts X-EXECD-ACCESS-TOKEN header`() {
        HttpClientProvider(
            ConnectionConfig.builder()
                .debug(true)
                .build(),
        ).use { provider ->
            val loggingInterceptor =
                provider.httpClient.interceptors
                    .filterIsInstance<okhttp3.logging.HttpLoggingInterceptor>()
                    .single()
            val field = okhttp3.logging.HttpLoggingInterceptor::class.java.getDeclaredField("headersToRedact")
            field.isAccessible = true
            @Suppress("UNCHECKED_CAST")
            val redacted = field.get(loggingInterceptor) as Set<String>
            assertTrue(
                redacted.any { it.equals("X-EXECD-ACCESS-TOKEN", ignoreCase = true) },
                "Expected X-EXECD-ACCESS-TOKEN to be in headersToRedact, got: $redacted",
            )
        }
    }

    @Test
    fun `cross-origin redirect strips the endpoint execd access token`() {
        MockWebServer().use { target ->
            MockWebServer().use { source ->
                target.enqueue(MockResponse().setBody("ok"))
                source.enqueue(
                    MockResponse()
                        .setResponseCode(302)
                        .setHeader("Location", target.url("/final")),
                )

                val endpoint =
                    SandboxEndpoint(
                        "${source.hostName}:${source.port}",
                        // Endpoint resolution and callers may spell the header any way they like.
                        headers = mapOf("x-execd-access-token" to "endpoint-secret"),
                    )

                HttpClientProvider(ConnectionConfig.builder().build()).use { provider ->
                    assertTrue(AdapterFactory(provider).createHealth(endpoint).ping("sandbox-id"))
                }

                // The endpoint that issued the token still authenticates with it...
                assertEquals("endpoint-secret", source.takeRequest().getHeader(EXECD_ACCESS_TOKEN_HEADER))
                // ...but the redirect target never receives it.
                assertNull(target.takeRequest().getHeader(EXECD_ACCESS_TOKEN_HEADER))
            }
        }
    }

    @Test
    fun `same-origin redirect keeps the endpoint execd access token`() {
        MockWebServer().use { server ->
            server.enqueue(MockResponse().setResponseCode(302).setHeader("Location", "/final"))
            server.enqueue(MockResponse().setBody("ok"))
            val endpoint =
                SandboxEndpoint(
                    "${server.hostName}:${server.port}",
                    headers = mapOf(EXECD_ACCESS_TOKEN_HEADER to "endpoint-secret"),
                )

            HttpClientProvider(ConnectionConfig.builder().build()).use { provider ->
                assertTrue(AdapterFactory(provider).createHealth(endpoint).ping("sandbox-id"))
            }

            repeat(2) {
                assertEquals("endpoint-secret", server.takeRequest().getHeader(EXECD_ACCESS_TOKEN_HEADER))
            }
        }
    }

    @Test
    fun `every provider client keeps the execd access token off later foreign hops`() {
        // OkHttp rebuilds each redirect hop from the application-level request,
        // so the token reappears on every hop and must be stripped on each one.
        MockWebServer().use { target ->
            MockWebServer().use { source ->
                HttpClientProvider(ConnectionConfig.builder().build()).use { provider ->
                    val clients =
                        mapOf(
                            "http" to provider.httpClient,
                            "transfer" to provider.transferClient,
                            "sse" to provider.sseClient,
                        )

                    for ((label, client) in clients) {
                        target.enqueue(MockResponse().setResponseCode(302).setHeader("Location", "/final"))
                        target.enqueue(MockResponse().setBody("ok"))
                        source.enqueue(
                            MockResponse()
                                .setResponseCode(302)
                                .setHeader("Location", target.url("/hop")),
                        )

                        // Isolated-session and transfer adapters set endpoint headers on the
                        // request itself rather than through an interceptor.
                        val request =
                            Request.Builder()
                                .url(source.url("/ping"))
                                .header(EXECD_ACCESS_TOKEN_HEADER, "endpoint-secret")
                                .build()
                        client.newCall(request).execute().use { response ->
                            assertEquals(200, response.code, label)
                        }

                        assertEquals("endpoint-secret", source.takeRequest().getHeader(EXECD_ACCESS_TOKEN_HEADER), label)
                        assertNull(target.takeRequest().getHeader(EXECD_ACCESS_TOKEN_HEADER), "$label /hop")
                        assertNull(target.takeRequest().getHeader(EXECD_ACCESS_TOKEN_HEADER), "$label /final")
                    }
                }
            }
        }
    }
}
