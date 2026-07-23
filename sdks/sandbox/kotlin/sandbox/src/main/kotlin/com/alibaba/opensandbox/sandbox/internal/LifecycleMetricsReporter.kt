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
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import okhttp3.Call
import okhttp3.Callback
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import org.slf4j.LoggerFactory
import java.io.IOException
import java.util.concurrent.TimeUnit

/**
 * Fire-and-forget reporter for SDK-side sandbox lifecycle metrics.
 *
 * Behavior matches the other language SDKs (Go/JS/Python/C#):
 * posts a small JSON body to `{baseUrl}/metrics/events` and never blocks
 * or surfaces errors to the caller. Honors both the connection-level
 * [ConnectionConfig.disableMetrics] opt-out and the
 * `OPENSANDBOX_DISABLE_METRICS=1` environment variable.
 */
internal object LifecycleMetricsReporter {
    private val logger = LoggerFactory.getLogger(LifecycleMetricsReporter::class.java)
    private val jsonMediaType = "application/json".toMediaType()

    /**
     * Best-effort reports the sandbox.create latency event. Never throws.
     *
     * @param connectionConfig connection configuration used to derive URL, headers, and opt-out
     * @param sandboxId optional created sandbox ID (omitted from payload when null)
     * @param image optional image or snapshot reference used for creation (omitted when null)
     * @param createDurationMs measured create latency in milliseconds (clamped to >= 0)
     * @param success whether the create flow succeeded end-to-end
     * @param client optional OkHttp client override, primarily for tests
     */
    @JvmStatic
    @JvmOverloads
    fun reportSandboxCreate(
        connectionConfig: ConnectionConfig,
        sandboxId: String?,
        image: String?,
        createDurationMs: Long,
        success: Boolean,
        client: OkHttpClient? = null,
    ) {
        if (connectionConfig.isMetricsDisabled()) {
            return
        }

        // Telemetry is best-effort and MUST NOT surface any exception to the caller.
        // In particular, this method is invoked from the Sandbox.create failure path;
        // any exception thrown from payload/URL/request construction (e.g. an invalid
        // configured baseUrl making Request.Builder.url(...) throw
        // IllegalArgumentException) would otherwise replace the original create
        // failure. Guard the whole body — not just the enqueue call — so every
        // telemetry failure is swallowed.
        try {
            val payload = buildPayload(sandboxId, image, createDurationMs, success)
            val url = connectionConfig.getBaseUrl().trimEnd('/') + "/metrics/events"

            val requestBuilder =
                Request.Builder()
                    .url(url)
                    .post(payload.toRequestBody(jsonMediaType))
                    .header("Content-Type", "application/json")
                    .header("User-Agent", connectionConfig.userAgent)

            val apiKey = connectionConfig.getApiKey()
            if (apiKey.isNotBlank()) {
                requestBuilder.header("OPEN-SANDBOX-API-KEY", apiKey)
            }
            for ((name, value) in connectionConfig.headers) {
                // Do not override the auth header if the user provided one via headers.
                if (name.equals("OPEN-SANDBOX-API-KEY", ignoreCase = true) ||
                    name.equals("Content-Type", ignoreCase = true) ||
                    name.equals("User-Agent", ignoreCase = true)
                ) {
                    requestBuilder.header(name, value)
                } else {
                    requestBuilder.addHeader(name, value)
                }
            }

            // When the caller supplies an OkHttpClient, they own its lifecycle. Otherwise
            // we create a client for this single fire-and-forget request and tear it down
            // in the callback so that high-churn create paths (e.g. SandboxPool prewarm)
            // do not accumulate OkHttp dispatcher threads or idle connections.
            val effectiveClient: OkHttpClient
            val sdkOwnsClient: Boolean
            if (client != null) {
                effectiveClient = client
                sdkOwnsClient = false
            } else {
                effectiveClient = defaultClient(connectionConfig)
                sdkOwnsClient = true
            }

            try {
                effectiveClient.newCall(requestBuilder.build()).enqueue(
                    object : Callback {
                        override fun onFailure(
                            call: Call,
                            e: IOException,
                        ) {
                            try {
                                logger.debug("Failed to report sandbox.create metrics: {}", e.message)
                            } finally {
                                if (sdkOwnsClient) shutdownQuietly(effectiveClient)
                            }
                        }

                        override fun onResponse(
                            call: Call,
                            response: Response,
                        ) {
                            try {
                                // Drain and close to release the connection back to the pool.
                                response.use { it.body?.string() }
                            } catch (_: Exception) {
                                // ignore — best effort
                            } finally {
                                if (sdkOwnsClient) shutdownQuietly(effectiveClient)
                            }
                        }
                    },
                )
            } catch (e: Exception) {
                // Never let telemetry disrupt the caller.
                logger.debug("Failed to enqueue sandbox.create metrics request: {}", e.message)
                if (sdkOwnsClient) shutdownQuietly(effectiveClient)
            }
        } catch (e: Exception) {
            // Payload/URL/request construction failed (e.g. invalid baseUrl). Swallow.
            logger.debug("Failed to build sandbox.create metrics request: {}", e.message)
        }
    }

    /**
     * Shuts down an SDK-owned OkHttpClient created solely for a single
     * telemetry request. Never throws.
     */
    private fun shutdownQuietly(client: OkHttpClient) {
        try {
            client.dispatcher.executorService.shutdown()
        } catch (_: Exception) {
            // ignore
        }
        try {
            client.connectionPool.evictAll()
        } catch (_: Exception) {
            // ignore
        }
    }

    /**
     * Builds a compact JSON payload for the sandbox.create metrics event.
     * `sandboxId` and `image` are omitted when null/blank to match the wire
     * format used by the other SDKs.
     */
    internal fun buildPayload(
        sandboxId: String?,
        image: String?,
        createDurationMs: Long,
        success: Boolean,
    ): String {
        val obj =
            buildJsonObject {
                put("eventType", JsonPrimitive("sandbox.create"))
                put("createDurationMs", JsonPrimitive(maxOf(0L, createDurationMs)))
                put("success", JsonPrimitive(success))
                if (!sandboxId.isNullOrBlank()) {
                    put("sandboxId", JsonPrimitive(sandboxId))
                }
                if (!image.isNullOrBlank()) {
                    put("image", JsonPrimitive(image))
                }
            }
        return obj.toString()
    }

    private fun defaultClient(connectionConfig: ConnectionConfig): OkHttpClient {
        val timeoutMs = maxOf(1L, connectionConfig.requestTimeout.toMillis())
        return OkHttpClient.Builder()
            .connectTimeout(timeoutMs, TimeUnit.MILLISECONDS)
            .readTimeout(timeoutMs, TimeUnit.MILLISECONDS)
            .writeTimeout(timeoutMs, TimeUnit.MILLISECONDS)
            .callTimeout(timeoutMs, TimeUnit.MILLISECONDS)
            .build()
    }
}
