/*
 * Copyright 2025 The OpenSandbox Authors
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
import java.time.Duration
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
    private val sseReadTimeoutFloor = Duration.ofSeconds(30)

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
                    .addNetworkInterceptor(ExecdAccessTokenRedirectInterceptor())
            if (config.enableTracing) {
                // Propagate the active trace context (W3C traceparent) so the
                // lifecycle server can join the same trace. No-op when there
                // is no active span in the current context.
                try {
                    builder.addInterceptor(
                        TraceContextInterceptor(GlobalOpenTelemetry.getPropagators().textMapPropagator),
                    )
                } catch (_: Throwable) {
                    // OpenTelemetry is best-effort; keep the original request path operational.
                }
            }
            return builder
        }

    private val httpClientLazy =
        lazy {
            baseBuilder
                .applyStandardTimeouts()
                .addRetryInterceptor()
                .addLoggingInterceptor()
                .build()
        }

    val httpClient: OkHttpClient by httpClientLazy

    // Transfers retain connect/read/write timeouts but use overallDeadline, not requestTimeout, as the total cap.
    private val transferClientLazy =
        lazy {
            // OkHttp rejects a call timeout above Int.MAX_VALUE millis, and a
            // positive deadline that truncates to 0 would silently disable the
            // cap, so the conversion is clamped instead of passed through.
            val transferDeadlineMillis =
                config.retryPolicy.overallDeadline
                    ?.toMillis()
                    ?.coerceIn(1L, Int.MAX_VALUE.toLong())
                    ?: 0L
            httpClient.newBuilder()
                .callTimeout(transferDeadlineMillis, TimeUnit.MILLISECONDS)
                .build()
        }

    val transferClient: OkHttpClient by transferClientLazy

    // Staged warmup owns health retries through its DelayQueue; disable both HTTP retry layers.
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

    // Retrying streamed command POSTs can execute them twice.
    // No total deadline; read timeout bounds gaps between SSE bytes.
    private val sseClientLazy =
        lazy {
            baseBuilder
                .connectTimeout(config.requestTimeout.toMillis(), TimeUnit.MILLISECONDS)
                .readTimeout(
                    maxOf(config.requestTimeout, sseReadTimeoutFloor).toMillis(),
                    TimeUnit.MILLISECONDS,
                )
                .writeTimeout(config.requestTimeout.toMillis(), TimeUnit.MILLISECONDS)
                .callTimeout(0, TimeUnit.MILLISECONDS)
                .retryOnConnectionFailure(false)
                .addInterceptor(ExtraHeadersInterceptor(getSseHeaders()))
                .addLoggingInterceptor()
                .build()
        }

    val sseClient: OkHttpClient by sseClientLazy

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
                    redactHeader("OPEN-SANDBOX-API-KEY")
                    redactHeader("Authorization")
                    redactHeader(SECURE_ACCESS_HEADER)
                    redactHeader(EXECD_ACCESS_TOKEN_HEADER)
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
            val request = chain.request()
            // An explicit header from ConnectionConfig.headers (matched
            // case-insensitively by OkHttp) wins over the configured API key.
            if (apiKey.isBlank() || request.header("OPEN-SANDBOX-API-KEY") != null) {
                return chain.proceed(request)
            }
            return chain.proceed(
                request.newBuilder()
                    .header("OPEN-SANDBOX-API-KEY", apiKey)
                    .build(),
            )
        }
    }

    // Runs after ExtraHeadersInterceptor so a caller-supplied IP header takes precedence.
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

    /**
     * Keeps the endpoint-scoped execd access token off cross-origin redirect hops.
     *
     * OkHttp drops `Authorization` on a redirect that changes the origin but replays
     * every other header, and it rebuilds each hop from the application-level request,
     * so the token must be stripped again on every hop. A network interceptor runs once
     * per exchange, which is the only hook that also sees those redirect hops.
     */
    private class ExecdAccessTokenRedirectInterceptor : Interceptor {
        override fun intercept(chain: Interceptor.Chain): Response {
            val request = chain.request()
            if (request.header(EXECD_ACCESS_TOKEN_HEADER) == null) {
                return chain.proceed(request)
            }
            // Compare each hop with the request that started the call (scheme, host,
            // and effective port), so later hops on another origin stay protected too.
            val origin = chain.call().request().url
            val sameOrigin =
                origin.scheme == request.url.scheme &&
                    origin.host == request.url.host &&
                    origin.port == request.url.port
            if (sameOrigin) {
                return chain.proceed(request)
            }
            return chain.proceed(request.newBuilder().removeHeader(EXECD_ACCESS_TOKEN_HEADER).build())
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
            val tracedRequest =
                try {
                    propagators.inject(Context.current(), headers, setter)
                    request.newBuilder().headers(headers.build()).build()
                } catch (_: Throwable) {
                    // Trace propagation is best-effort and must never fail an HTTP request.
                    request
                }
            return chain.proceed(tracedRequest)
        }
    }

    /**
     * Closes the underlying HTTP client and releases resources.
     */
    override fun close() {
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
