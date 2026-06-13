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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.service

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.models.execd.pty.PtyMode
import com.alibaba.opensandbox.sandbox.domain.models.execd.pty.PtySession
import com.alibaba.opensandbox.sandbox.domain.models.execd.pty.PtySessionStatus
import com.alibaba.opensandbox.sandbox.domain.models.execd.pty.PtyWebSocket
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.services.Pty
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.jsonParser
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import okhttp3.Headers.Companion.toHeaders
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.slf4j.LoggerFactory

/**
 * Implementation of [Pty] backed by execd's PTY HTTP endpoints.
 *
 * execd's PTY routes are not part of the OpenAPI specs (the interactive channel is a
 * WebSocket), so this adapter is handwritten transport over the shared OkHttp client,
 * following the same endpoint/header wiring as the other execd adapters.
 */
internal class PtyAdapter(
    private val httpClientProvider: HttpClientProvider,
    private val execdEndpoint: SandboxEndpoint,
) : Pty {
    companion object {
        private const val PTY_PATH = "/pty"
        private val JSON_MEDIA_TYPE = "application/json".toMediaType()
    }

    private val logger = LoggerFactory.getLogger(PtyAdapter::class.java)
    private val execdBaseUrl = "${httpClientProvider.config.protocol}://${execdEndpoint.endpoint}"
    private val execdApiClient =
        httpClientProvider.httpClient.newBuilder()
            .addInterceptor { chain ->
                val requestBuilder = chain.request().newBuilder()
                execdEndpoint.headers.forEach { (key, value) ->
                    requestBuilder.header(key, value)
                }
                chain.proceed(requestBuilder.build())
            }
            .build()

    override fun createSession(
        cwd: String?,
        command: String?,
    ): PtySession {
        try {
            val body = jsonParser.encodeToString(CreatePtySessionRequest(cwd, command))
            val request =
                Request.Builder()
                    .url("$execdBaseUrl$PTY_PATH")
                    .post(body.toRequestBody(JSON_MEDIA_TYPE))
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()
            execdApiClient.newCall(request).execute().use { response ->
                val payload = response.body?.string().orEmpty()
                if (!response.isSuccessful) {
                    throw SandboxApiException(
                        message = "Failed to create PTY session. Status code: ${response.code}, Body: $payload",
                        statusCode = response.code,
                    )
                }
                val parsed = jsonParser.decodeFromString<CreatePtySessionResponse>(payload)
                return PtySession(parsed.sessionId)
            }
        } catch (e: Exception) {
            logger.error("Failed to create PTY session", e)
            throw e.toSandboxException()
        }
    }

    override fun getSession(sessionId: String): PtySessionStatus {
        try {
            val request =
                Request.Builder()
                    .url("$execdBaseUrl$PTY_PATH/$sessionId")
                    .get()
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()
            execdApiClient.newCall(request).execute().use { response ->
                val payload = response.body?.string().orEmpty()
                if (!response.isSuccessful) {
                    throw SandboxApiException(
                        message = "Failed to get PTY session $sessionId. Status code: ${response.code}, Body: $payload",
                        statusCode = response.code,
                    )
                }
                val parsed = jsonParser.decodeFromString<PtySessionStatusResponse>(payload)
                return PtySessionStatus(parsed.sessionId, parsed.running, parsed.outputOffset)
            }
        } catch (e: Exception) {
            logger.error("Failed to get PTY session {}", sessionId, e)
            throw e.toSandboxException()
        }
    }

    override fun deleteSession(sessionId: String) {
        try {
            val request =
                Request.Builder()
                    .url("$execdBaseUrl$PTY_PATH/$sessionId")
                    .delete()
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()
            execdApiClient.newCall(request).execute().use { response ->
                if (!response.isSuccessful) {
                    val payload = response.body?.string().orEmpty()
                    throw SandboxApiException(
                        message = "Failed to delete PTY session $sessionId. Status code: ${response.code}, Body: $payload",
                        statusCode = response.code,
                    )
                }
            }
        } catch (e: Exception) {
            logger.error("Failed to delete PTY session {}", sessionId, e)
            throw e.toSandboxException()
        }
    }

    override fun webSocket(
        sessionId: String,
        mode: PtyMode,
        since: Long?,
        takeover: Boolean,
    ): PtyWebSocket {
        val scheme = if (httpClientProvider.config.protocol.equals("https", ignoreCase = true)) "wss" else "ws"
        val params =
            buildList {
                if (mode == PtyMode.PIPE) add("pty=0")
                if (since != null) add("since=$since")
                if (takeover) add("takeover=1")
            }
        val query = if (params.isEmpty()) "" else "?" + params.joinToString("&")
        val url = "$scheme://${execdEndpoint.endpoint}$PTY_PATH/$sessionId/ws$query"
        // Carry the same routing/auth headers the REST calls send so callers can complete the
        // WebSocket handshake against header-mode ingress or secure-access endpoints.
        return PtyWebSocket(url, execdEndpoint.headers)
    }

    @Serializable
    private data class CreatePtySessionRequest(
        val cwd: String? = null,
        val command: String? = null,
    )

    @Serializable
    private data class CreatePtySessionResponse(
        @SerialName("session_id") val sessionId: String,
    )

    @Serializable
    private data class PtySessionStatusResponse(
        @SerialName("session_id") val sessionId: String,
        @SerialName("running") val running: Boolean = false,
        @SerialName("output_offset") val outputOffset: Long = 0,
    )
}
