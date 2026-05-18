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
import okhttp3.ConnectionPool
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
        get() =
            OkHttpClient.Builder()
                .connectionPool(connectionPool)
                .addInterceptor(UserAgentInterceptor(config.userAgent))
                .addInterceptor(ExtraHeadersInterceptor(config.headers))

    // 1. Explicit lazy definition to allow checking initialization status
    private val httpClientLazy =
        lazy {
            baseBuilder
                .applyStandardTimeouts()
                .addLoggingInterceptor()
                .build()
        }

    val httpClient: OkHttpClient by httpClientLazy

    // 2. Explicit lazy definition for authenticated client
    private val authenticatedClientLazy =
        lazy {
            baseBuilder
                .applyStandardTimeouts()
                .addInterceptor(AuthenticationInterceptor(config.getApiKey())) // Add auth before logging
                .addLoggingInterceptor()
                .build()
        }

    val authenticatedClient: OkHttpClient by authenticatedClientLazy

    // 3. Explicit lazy definition for SSE client
    private val sseClientLazy =
        lazy {
            baseBuilder
                .connectTimeout(config.requestTimeout.toMillis(), TimeUnit.MILLISECONDS)
                .readTimeout(0, TimeUnit.MILLISECONDS)
                .writeTimeout(config.requestTimeout.toMillis(), TimeUnit.MILLISECONDS)
                .callTimeout(0, TimeUnit.MILLISECONDS)
                .addInterceptor(ExtraHeadersInterceptor(getSseHeaders()))
                .apply { sseInterceptors.forEach { addInterceptor(it) } }
                .addLoggingInterceptor()
                .build()
        }

    val sseClient: OkHttpClient by sseClientLazy

    internal val sseInterceptors: MutableList<Interceptor> = mutableListOf()

    // --- Helper Extensions ---

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
