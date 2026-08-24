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

package com.alibaba.opensandbox.sandbox.config

import com.alibaba.opensandbox.sandbox.transport.RetryPolicy
import okhttp3.ConnectionPool
import java.time.Duration

/**
 * Sandbox operations connection configuration.
 */
class ConnectionConfig private constructor(
    /** API key for authentication with sandbox service */
    private val apiKey: String?,
    /** Base URL for the sandbox management API */
    private val domain: String?,
    /** Protocol to use (http/https) */
    val protocol: String,
    /** Timeout for HTTP requests to the management API */
    val requestTimeout: Duration,
    /** Enable debug logging for HTTP requests */
    val debug: Boolean = false,
    /** user agent */
    val userAgent: String = DEFAULT_USER_AGENT,
    /** User defined headers */
    val headers: Map<String, String> = mutableMapOf(),
    /** Connection pool (optional) */
    val connectionPool: ConnectionPool?,
    /** Whether the connection pool is managed by the user */
    val connectionPoolManagedByUser: Boolean,
    /**
     * Use sandbox server as proxy for process execd requests.
     * Useful when the client SDK cannot access the created sandbox directly.
     */
    val useServerProxy: Boolean = false,
    /** TTL for cached endpoint entries. Default: 10 minutes. */
    val endpointCacheTtl: Duration = Duration.ofSeconds(600),
    /** Maximum number of cached endpoint entries. Default: 1024. */
    val endpointCacheSize: Int = 1024,
    /** Disable endpoint caching entirely. */
    val endpointCacheDisabled: Boolean = false,
    /**
     * Disable best-effort SDK telemetry (sandbox.create latency reports).
     *
     * Also honored via the `OPENSANDBOX_DISABLE_METRICS=1` environment variable.
     */
    val disableMetrics: Boolean = false,
    /**
     * Enable OpenTelemetry tracing for the client-side sandbox pool warmup path.
     *
     * Off by default. When enabled, each pool warmup creates an OpenTelemetry
     * trace (`pool.warmup` root span plus per-phase spans) and the active
     * trace context is propagated to lifecycle requests via the W3C
     * `traceparent` header. Tracing is best-effort: without an
     * OpenTelemetry SDK + exporter on the classpath, all span calls are
     * no-ops and nothing is exported.
     */
    val enableTracing: Boolean = false,
    /**
     * Retry policy applied to non-streaming requests. Enabled by default; pass
     * [RetryPolicy.disabled] to disable SDK-policy retries and fall back to
     * OkHttp's built-in connection recovery. SSE / streaming requests bypass
     * the SDK retry policy and disable built-in connection recovery regardless
     * of this value because they cannot be safely replayed.
     */
    val retryPolicy: RetryPolicy = RetryPolicy(),
    /** Internal override used by single-attempt operations to disable OkHttp connection recovery. */
    internal val retryOnConnectionFailure: Boolean = true,
    /** Internal transport mode used only by staged pool warmup health probes. */
    internal val singleAttemptHealthChecks: Boolean = false,
) {
    /**
     * Creates a copy of this ConnectionConfig without copying the connectionPool.
     * The returned config will have connectionPool set to null and connectionPoolManagedByUser set to false.
     */
    fun copyWithoutConnectionPool(): ConnectionConfig =
        ConnectionConfig(
            apiKey = this.apiKey,
            domain = this.domain,
            protocol = this.protocol,
            requestTimeout = this.requestTimeout,
            debug = this.debug,
            userAgent = this.userAgent,
            headers = this.headers,
            connectionPool = null,
            connectionPoolManagedByUser = false,
            useServerProxy = this.useServerProxy,
            endpointCacheTtl = this.endpointCacheTtl,
            endpointCacheSize = this.endpointCacheSize,
            endpointCacheDisabled = this.endpointCacheDisabled,
            disableMetrics = this.disableMetrics,
            enableTracing = this.enableTracing,
            retryPolicy = this.retryPolicy,
            retryOnConnectionFailure = this.retryOnConnectionFailure,
            singleAttemptHealthChecks = this.singleAttemptHealthChecks,
        )

    /**
     * Creates a copy of this ConnectionConfig that uses [connectionPool] and
     * marks it as SDK-managed (evicted when the owning component closes).
     *
     * Internal to the SDK: only [com.alibaba.opensandbox.sandbox.pool.SandboxPool]
     * injects its pool-created shared pool this way and evicts it on shutdown.
     * It is not public API — callers cannot rely on the eviction promise
     * because [HttpClientProvider] only evicts pools it created itself.
     */
    internal fun copyWithConnectionPool(connectionPool: ConnectionPool): ConnectionConfig =
        ConnectionConfig(
            apiKey = this.apiKey,
            domain = this.domain,
            protocol = this.protocol,
            requestTimeout = this.requestTimeout,
            debug = this.debug,
            userAgent = this.userAgent,
            headers = this.headers,
            connectionPool = connectionPool,
            connectionPoolManagedByUser = false,
            useServerProxy = this.useServerProxy,
            endpointCacheTtl = this.endpointCacheTtl,
            endpointCacheSize = this.endpointCacheSize,
            endpointCacheDisabled = this.endpointCacheDisabled,
            disableMetrics = this.disableMetrics,
            enableTracing = this.enableTracing,
            retryPolicy = this.retryPolicy,
            retryOnConnectionFailure = this.retryOnConnectionFailure,
            singleAttemptHealthChecks = this.singleAttemptHealthChecks,
        )

    /**
     * Creates an internal transport configuration that performs exactly one HTTP attempt.
     * Public [RetryPolicy.disabled] semantics remain unchanged and still allow OkHttp recovery.
     */
    internal fun copyForSingleAttempt(): ConnectionConfig =
        ConnectionConfig(
            apiKey = this.apiKey,
            domain = this.domain,
            protocol = this.protocol,
            requestTimeout = this.requestTimeout,
            debug = this.debug,
            userAgent = this.userAgent,
            headers = this.headers,
            connectionPool = this.connectionPool,
            connectionPoolManagedByUser = this.connectionPoolManagedByUser,
            useServerProxy = this.useServerProxy,
            endpointCacheTtl = this.endpointCacheTtl,
            endpointCacheSize = this.endpointCacheSize,
            endpointCacheDisabled = this.endpointCacheDisabled,
            disableMetrics = this.disableMetrics,
            enableTracing = this.enableTracing,
            retryPolicy = RetryPolicy.disabled(),
            retryOnConnectionFailure = false,
            singleAttemptHealthChecks = this.singleAttemptHealthChecks,
        )

    /**
     * Derives the connection configuration used by a staged warmup Sandbox.
     *
     * Only health probes become single-attempt requests. Other operations retain
     * the caller's retry policy, and the original configuration is unchanged.
     */
    internal fun copyForStagedWarmup(): ConnectionConfig =
        ConnectionConfig(
            apiKey = this.apiKey,
            domain = this.domain,
            protocol = this.protocol,
            requestTimeout = this.requestTimeout,
            debug = this.debug,
            userAgent = this.userAgent,
            headers = this.headers,
            connectionPool = this.connectionPool,
            connectionPoolManagedByUser = this.connectionPoolManagedByUser,
            useServerProxy = this.useServerProxy,
            endpointCacheTtl = this.endpointCacheTtl,
            endpointCacheSize = this.endpointCacheSize,
            endpointCacheDisabled = this.endpointCacheDisabled,
            disableMetrics = this.disableMetrics,
            enableTracing = this.enableTracing,
            retryPolicy = this.retryPolicy,
            retryOnConnectionFailure = this.retryOnConnectionFailure,
            singleAttemptHealthChecks = true,
        )

    companion object {
        private const val DEFAULT_DOMAIN = "localhost:8080"
        private const val DEFAULT_PROTOCOL = "http"
        private const val ENV_API_KEY = "OPEN_SANDBOX_API_KEY"
        private const val ENV_DOMAIN = "OPEN_SANDBOX_DOMAIN"
        internal const val ENV_DISABLE_METRICS = "OPENSANDBOX_DISABLE_METRICS"

        private const val DEFAULT_USER_AGENT = "OpenSandbox-Kotlin-SDK/1.0.19"
        private const val API_VERSION = "v1"

        @JvmStatic
        fun builder(): Builder = Builder()
    }

    /**
     * Returns whether SDK telemetry (sandbox.create latency reports) should
     * be skipped. Honors both the [disableMetrics] flag and the
     * `OPENSANDBOX_DISABLE_METRICS=1` environment variable.
     */
    fun isMetricsDisabled(): Boolean {
        if (disableMetrics) return true
        val envValue = System.getenv(ENV_DISABLE_METRICS)?.trim()
        return envValue == "1"
    }

    fun getApiKey(): String {
        return this.apiKey ?: System.getenv(ENV_API_KEY) ?: ""
    }

    fun getDomain(): String {
        return this.domain ?: System.getenv(ENV_DOMAIN) ?: DEFAULT_DOMAIN
    }

    fun getBaseUrl(): String {
        val currentDomain = getDomain()
        // Python semantics:
        // - If `domain` includes a scheme, treat it as a full base URL (without `/v1`) and append `/v1`.
        // - If `domain` does not include a scheme, build `protocol://domain/v1`.
        // Also normalize trailing slashes and avoid duplicating `/v1`.
        if (currentDomain.startsWith("http://") || currentDomain.startsWith("https://")) {
            val trimmed = currentDomain.removeSuffix("/")
            return if (trimmed.endsWith("/$API_VERSION")) trimmed else "$trimmed/$API_VERSION"
        }
        val trimmed = currentDomain.removeSuffix("/")
        return if (trimmed.endsWith(
                "/$API_VERSION",
            )
        ) {
            "$protocol://${trimmed.removeSuffix("/$API_VERSION")}/$API_VERSION"
        } else {
            "$protocol://$trimmed/$API_VERSION"
        }
    }

    /**
     * Builder for [ConnectionConfig].
     *
     * This builder is part of the public SDK surface and is intended to be used directly by end users.
     *
     * ### Defaults & environment variables
     * - If `apiKey` is not provided, the SDK will read it from environment variable `OPEN_SANDBOX_API_KEY`.
     * - If `domain` is not provided, the SDK will read it from environment variable `OPEN_SANDBOX_DOMAIN`,
     *   falling back to `localhost:8080`.
     *
     * ### Lifecycle / resource ownership
     * - If you do **not** provide a custom [ConnectionPool], the SDK creates and owns a default one
     *   per Sandbox/Manager instance. Calling `Sandbox.close()` / `SandboxManager.close()` will
     *   close SDK-owned HTTP clients and release the SDK-owned connection pool.
     * - If you **do** provide a [ConnectionPool] via [connectionPool], it is treated as user-owned
     *   and will **not** be evicted by the SDK on close.
     *
     * ### Notes
     * - `domain` may include a scheme (e.g. `https://example.com`); in that case the SDK will ignore [protocol]
     *   and append `/$API_VERSION` automatically when constructing the base URL.
     */
    class Builder internal constructor() {
        private var apiKey: String? = null

        private var domain: String? = null

        private var protocol: String = DEFAULT_PROTOCOL

        private var requestTimeout: Duration = Duration.ofSeconds(30)

        private var debug: Boolean = false

        private var headers: Map<String, String> = mutableMapOf()

        private var connectionPool: ConnectionPool? = null

        private var connectionPoolManagedByUser: Boolean = false

        private var useServerProxy: Boolean = false
        private var endpointCacheTtl: Duration = Duration.ofSeconds(600)
        private var endpointCacheSize: Int = 1024
        private var endpointCacheDisabled: Boolean = false
        private var disableMetrics: Boolean = false
        private var enableTracing: Boolean = false
        private var retryPolicy: RetryPolicy = RetryPolicy()

        /**
         * Use sandbox server as proxy for process execd requests.
         * Useful when the client SDK cannot access the created sandbox directly.
         */
        fun useServerProxy(useServerProxy: Boolean): Builder {
            this.useServerProxy = useServerProxy
            return this
        }

        /** Set endpoint cache TTL. */
        fun endpointCacheTtl(ttl: Duration): Builder {
            this.endpointCacheTtl = ttl
            return this
        }

        /** Set endpoint cache max size. */
        fun endpointCacheSize(size: Int): Builder {
            this.endpointCacheSize = size
            return this
        }

        /** Disable endpoint caching. */
        fun endpointCacheDisabled(disabled: Boolean): Builder {
            this.endpointCacheDisabled = disabled
            return this
        }

        /**
         * Disable best-effort SDK telemetry (sandbox.create latency reports).
         *
         * Also honored via `OPENSANDBOX_DISABLE_METRICS=1`.
         */
        @JvmOverloads
        fun disableMetrics(disabled: Boolean = true): Builder {
            this.disableMetrics = disabled
            return this
        }

        /**
         * Enable OpenTelemetry tracing for the client-side sandbox pool warmup path.
         *
         * Off by default; pass `true` to opt in. Tracing is best-effort and no-ops
         * unless an OpenTelemetry SDK + exporter is on the classpath.
         */
        fun enableTracing(enable: Boolean = true): Builder {
            this.enableTracing = enable
            return this
        }

        /**
         * Set the API key used for authentication.
         *
         * If not set, the SDK falls back to environment variable `OPEN_SANDBOX_API_KEY`.
         */
        fun apiKey(apiKey: String): Builder {
            require(apiKey.isNotBlank()) { "API key cannot be blank" }
            this.apiKey = apiKey
            return this
        }

        /**
         * Set the API domain (host[:port]) or a full base URL.
         *
         * Examples:
         * - `pre-agent-sandbox.alibaba-inc.com`
         * - `localhost:8080`
         * - `https://pre-agent-sandbox.alibaba-inc.com` (scheme included; [protocol] will be ignored)
         *
         * If not set, the SDK falls back to environment variable `OPEN_SANDBOX_DOMAIN`
         * and then `localhost:8080`.
         */
        fun domain(domain: String): Builder {
            require(domain.isNotBlank()) { "Domain cannot be blank" }
            this.domain = domain
            return this
        }

        /**
         * Sets the protocol
         * Defaults to "http".
         *
         * Note: if [domain] includes a scheme (starts with `http://` or `https://`),
         * the SDK will use that and ignore this value when building the base URL.
         */
        fun protocol(protocol: String): Builder {
            this.protocol = protocol.lowercase()
            return this
        }

        /**
         * Sets the request timeout used by the management API HTTP client.
         *
         * Must be a positive duration.
         */
        fun requestTimeout(requestTimeout: Duration): Builder {
            require(!requestTimeout.isNegative && !requestTimeout.isZero) {
                "Request timeout must be positive, got: $requestTimeout"
            }
            this.requestTimeout = requestTimeout
            return this
        }

        /**
         * Provide a custom OkHttp [ConnectionPool].
         *
         * Ownership semantics:
         * - When you call this method, the pool is considered user-managed, and the SDK will not
         *   evict it on close.
         */
        fun connectionPool(connectionPool: ConnectionPool): Builder {
            this.connectionPool = connectionPool
            this.connectionPoolManagedByUser = true
            return this
        }

        /**
         * Set the retry policy applied to non-streaming requests.
         *
         * Retries are enabled by default (idempotent methods only). Pass
         * [RetryPolicy.disabled] to disable SDK-policy retries and fall back to
         * OkHttp's built-in connection recovery. SSE / streaming requests bypass
         * the SDK retry policy and disable built-in connection recovery because
         * they cannot be safely replayed.
         */
        fun retryPolicy(retryPolicy: RetryPolicy): Builder {
            this.retryPolicy = retryPolicy
            return this
        }

        /**
         * Enable or disable HTTP request logging (headers).
         *
         * This is intended for local debugging. Sensitive headers will be redacted.
         */
        fun debug(enable: Boolean = true): Builder {
            this.debug = enable
            return this
        }

        /**
         * Set extra headers that will be sent with every SDK request.
         *
         * Note: authentication header is managed by the SDK; you normally should not set
         * `OPEN-SANDBOX-API-KEY` manually here.
         */
        fun headers(headers: Map<String, String>): Builder {
            this.headers = headers
            return this
        }

        /**
         * Convenience DSL for setting extra headers.
         *
         * Example:
         * ```
         * ConnectionConfig.builder()
         *   .headers {
         *     put("X-Request-ID", "trace-123")
         *   }
         *   .build()
         * ```
         */
        fun headers(configure: MutableMap<String, String>.() -> Unit): Builder {
            val map = mutableMapOf<String, String>()
            map.configure()
            this.headers = map
            return this
        }

        /**
         * Add a single extra header.
         *
         * This is equivalent to mutating [headers] and overwriting the value for the same key.
         */
        fun addHeader(
            key: String,
            value: String,
        ): Builder {
            require(key.isNotBlank()) { "Header key cannot be blank" }
            val mutableHeaders = this.headers.toMutableMap()
            mutableHeaders[key] = value
            this.headers = mutableHeaders
            return this
        }

        /**
         * Build an immutable [ConnectionConfig].
         */
        fun build(): ConnectionConfig {
            return ConnectionConfig(
                apiKey = apiKey,
                domain = domain,
                protocol = protocol,
                requestTimeout = requestTimeout,
                debug = debug,
                userAgent = DEFAULT_USER_AGENT,
                headers = headers,
                connectionPool = connectionPool,
                connectionPoolManagedByUser = connectionPoolManagedByUser,
                useServerProxy = useServerProxy,
                endpointCacheTtl = endpointCacheTtl,
                endpointCacheSize = endpointCacheSize,
                endpointCacheDisabled = endpointCacheDisabled,
                disableMetrics = disableMetrics,
                enableTracing = enableTracing,
                retryPolicy = retryPolicy,
                retryOnConnectionFailure = true,
                singleAttemptHealthChecks = false,
            )
        }
    }
}
