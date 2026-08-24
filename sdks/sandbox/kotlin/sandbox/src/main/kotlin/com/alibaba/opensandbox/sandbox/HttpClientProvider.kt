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
import com.alibaba.opensandbox.sandbox.domain.models.execd.SECURE_ACCESS_HEADER
import com.alibaba.opensandbox.sandbox.transport.RetryInterceptor
import io.opentelemetry.api.GlobalOpenTelemetry
import io.opentelemetry.context.Context
import io.opentelemetry.context.propagation.TextMapPropagator
import io.opentelemetry.context.propagation.TextMapSetter
import okhttp3.ConnectionPool
import okhttp3.Headers
import okhttp3.Interceptor
import okhttp3.OkHttpClient
import okhttp3.Response
import okhttp3.logging.HttpLoggingInterceptor
import org.slf4j.LoggerFactory
import java.util.concurrent.TimeUnit

/**
 * Provider that manages HTTP client instances with proper configuration.
 */
class HttpClientProvider(
    val config: ConnectionConfig,
) : AutoCloseable {
    private val logger = LoggerFactory.getLogger(HttpClientProvider::class.java)

    private val defaultMaxIdleConnections = 32
    private val defaultKeepAliveDurationSeconds = 30L

    private val connectionPool =
        config.connectionPool ?: ConnectionPool(defaultMaxIdleConnections, defaultKeepAliveDurationSeconds, TimeUnit.SECONDS)

    private val connectionPoolOwnedBySdk: Boolean = config.connectionPool == null

    private val baseBuilder: OkHttpClient.Builder
        get() {
            val builder =
                OkHttpClient.Builder()
                    .connectionPool(connectionPool)
                    .addInterceptor(UserAgentInterceptor(config.userAgent))
                    .addInterceptor(ExtraHeadersInterceptor(config.headers))
                    .addInterceptor(ClientIpInterceptor { ClientIpDetector.clientIp() })
            if (config.enableTracing) {
                // Propagate the active trace context (W3C traceparent) so the
                // lifecycle server can join the same trace. No-op when there
                // is no active span in the current context.
                builder.addInterceptor(TraceContextInterceptor(GlobalOpenTelemetry.getPropagators().textMapPropagator))
            }
            return builder
        }

    // 1. Explicit lazy definition to allow checking initialization status
    private val httpClientLazy =
        lazy {
            baseBuilder
                .applyStandardTimeouts()
                .addRetryInterceptor()
                .addLoggingInterceptor()
                .build()
        }

    val httpClient: OkHttpClient by httpClientLazy

    // A staged warmup already retries health through its DelayQueue. Clone the
    // regular client so the warmup-only probe shares its dispatcher, connection
    // pool, headers, tracing, timeouts, and logging while skipping both retry
    // owners for exactly one HTTP attempt.
    private val singleAttemptClientLazy =
        lazy {
            httpClient.newBuilder()
                .apply {
                    interceptors().removeAll { it is RetryInterceptor }
                }
                .retryOnConnectionFailure(false)
                .build()
        }

    internal val singleAttemptClient: OkHttpClient by singleAttemptClientLazy

    // 2. Explicit lazy definition for authenticated client
    private val authenticatedClientLazy =
        lazy {
            baseBuilder
                .applyStandardTimeouts()
                .addRetryInterceptor()
                .addInterceptor(AuthenticationInterceptor(config.getApiKey())) // Add auth before logging
                .addLoggingInterceptor()
                .build()
        }

    val authenticatedClient: OkHttpClient by authenticatedClientLazy

    // 3. Explicit lazy definition for SSE client
    //
    // The SSE client deliberately disables all automatic retries: streaming
    // command POSTs are not safely replayable and could start a command twice
    // if the connection fails after the server accepts the request.
    private val sseClientLazy =
        lazy {
            baseBuilder
                .connectTimeout(config.requestTimeout.toMillis(), TimeUnit.MILLISECONDS)
                .readTimeout(0, TimeUnit.MILLISECONDS)
                .writeTimeout(config.requestTimeout.toMillis(), TimeUnit.MILLISECONDS)
                .callTimeout(0, TimeUnit.MILLISECONDS)
                .retryOnConnectionFailure(false)
                .addInterceptor(ExtraHeadersInterceptor(getSseHeaders()))
                .addLoggingInterceptor()
                .build()
        }

    val sseClient: OkHttpClient by sseClientLazy

    // --- Helper Extensions ---

    /**
     * Installs [RetryInterceptor] and disables OkHttp's built-in connection
     * recovery, so the SDK is the single owner of retry behaviour (matching
     * the Python transport wrapper single-owner model).
     *
     * When the policy does not require the interceptor (wrapsTransport() is
     * false), this is a no-op — the caller relies on OkHttp defaults.
     */
    private fun OkHttpClient.Builder.addRetryInterceptor(): OkHttpClient.Builder {
        if (!config.retryOnConnectionFailure) {
            retryOnConnectionFailure(false)
            return this
        }
        if (config.retryPolicy.wrapsTransport()) {
            retryOnConnectionFailure(false)
            addInterceptor(RetryInterceptor(config.retryPolicy))
        }
        return this
    }

    private fun OkHttpClient.Builder.applyStandardTimeouts(): OkHttpClient.Builder {
        val timeout = config.requestTimeout.toMillis()
        return this.connectTimeout(timeout, TimeUnit.MILLISECONDS)
            .readTimeout(timeout, TimeUnit.MILLISECONDS)
            .writeTimeout(timeout, TimeUnit.MILLISECONDS)
            .callTimeout(timeout, TimeUnit.MILLISECONDS)
    }

    private fun OkHttpClient.Builder.addLoggingInterceptor(): OkHttpClient.Builder {
        if (config.debug) {
            val loggingInterceptor =
                HttpLoggingInterceptor { message ->
                    logger.debug(message)
                }.apply {
                    level = HttpLoggingInterceptor.Level.HEADERS
                    // Redact sensitive headers in logs
                    redactHeader("OPEN-SANDBOX-API-KEY")
                    redactHeader("Authorization")
                    redactHeader(SECURE_ACCESS_HEADER)
                }
            addInterceptor(loggingInterceptor)
        }
        return this
    }

    private fun getSseHeaders(): Map<String, String> {
        return mapOf(
            "Accept" to "text/event-stream",
            "Cache-Control" to "no-cache",
        )
    }

    // --- Interceptors ---

    private class UserAgentInterceptor(private val userAgent: String) : Interceptor {
        override fun intercept(chain: Interceptor.Chain): Response {
            return chain.proceed(
                chain.request().newBuilder()
                    .header("User-Agent", userAgent)
                    .build(),
            )
        }
    }

    private class AuthenticationInterceptor(private val apiKey: String) : Interceptor {
        override fun intercept(chain: Interceptor.Chain): Response {
            return chain.proceed(
                chain.request().newBuilder()
                    .header("OPEN-SANDBOX-API-KEY", apiKey)
                    .build(),
            )
        }
    }

    // Best-effort: attach the SDK host's own IP so the server can see the
    // client's self-reported address. Runs after ExtraHeadersInterceptor so a
    // user-supplied value (matched case-insensitively by OkHttp) is preserved,
    // and is skipped silently when the IP cannot be determined.
    private class ClientIpInterceptor(private val ipSupplier: () -> String) : Interceptor {
        override fun intercept(chain: Interceptor.Chain): Response {
            val request = chain.request()
            if (request.header(ClientIpDetector.CLIENT_IP_HEADER) != null) {
                return chain.proceed(request)
            }
            val ip = ipSupplier()
            if (ip.isEmpty()) {
                return chain.proceed(request)
            }
            return chain.proceed(
                request.newBuilder()
                    .header(ClientIpDetector.CLIENT_IP_HEADER, ip)
                    .build(),
            )
        }
    }

    private class ExtraHeadersInterceptor(private val headers: Map<String, String>) : Interceptor {
        override fun intercept(chain: Interceptor.Chain): Response {
            if (headers.isEmpty()) return chain.proceed(chain.request())

            val builder = chain.request().newBuilder()
            headers.forEach { (name, value) ->
                builder.addHeader(name, value)
            }
            return chain.proceed(builder.build())
        }
    }

    /**
     * Injects the W3C `traceparent` / `tracestate` headers of the current
     * OpenTelemetry context into every request. When no span is active the
     * propagator injects nothing and the request passes through unchanged.
     */
    private class TraceContextInterceptor(
        private val propagators: TextMapPropagator,
    ) : Interceptor {
        private val setter =
            TextMapSetter<Headers.Builder> { carrier: Headers.Builder?, key, value ->
                carrier?.set(key, value)
            }

        override fun intercept(chain: Interceptor.Chain): Response {
            val request = chain.request()
            val headers = request.headers.newBuilder()
            propagators.inject(Context.current(), headers, setter)
            return chain.proceed(request.newBuilder().headers(headers.build()).build())
        }
    }

    // --- Cleanup ---

    /**
     * Closes the underlying HTTP client and releases resources.
     */
    override fun close() {
        // Now we can pass the specific backing fields to check initialization
        shutdownClientQuietly(httpClientLazy, "http client")
        shutdownClientQuietly(authenticatedClientLazy, "authenticated client")
        shutdownClientQuietly(sseClientLazy, "sse client")

        if (connectionPoolOwnedBySdk && !config.connectionPoolManagedByUser) {
            try {
                connectionPool.evictAll()
            } catch (e: Exception) {
                logger.warn("Error evicting connection pool", e)
            }
        }
    }

    private fun shutdownClientQuietly(
        lazyClient: Lazy<OkHttpClient>,
        name: String,
    ) {
        if (lazyClient.isInitialized()) {
            try {
                val client = lazyClient.value
                client.dispatcher.cancelAll()
                client.dispatcher.executorService.shutdownNow()
            } catch (e: Exception) {
                logger.warn("Error closing $name", e)
            }
        }
    }
}
