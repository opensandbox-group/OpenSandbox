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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter

import com.alibaba.opensandbox.sandbox.api.infrastructure.ClientError
import com.alibaba.opensandbox.sandbox.api.infrastructure.ClientException
import com.alibaba.opensandbox.sandbox.api.infrastructure.ServerError
import com.alibaba.opensandbox.sandbox.api.infrastructure.ServerException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxConnectionException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError.Companion.UNEXPECTED_RESPONSE
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxInternalException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxRateLimitException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxTimeoutException
import com.alibaba.opensandbox.sandbox.transport.RetryDeadlineExceededException
import com.alibaba.opensandbox.sandbox.transport.RetryPolicy
import com.alibaba.opensandbox.sandbox.transport.parseRetryAfter
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.decodeFromJsonElement
import kotlinx.serialization.json.encodeToJsonElement
import okhttp3.Response
import java.io.IOException
import java.io.InterruptedIOException
import java.net.ConnectException
import java.net.NoRouteToHostException
import java.net.SocketTimeoutException
import java.net.UnknownHostException
import java.time.Duration
import javax.net.ssl.SSLException
import com.alibaba.opensandbox.sandbox.api.diagnostic.infrastructure.ClientError as DiagnosticClientError
import com.alibaba.opensandbox.sandbox.api.diagnostic.infrastructure.ClientException as DiagnosticClientException
import com.alibaba.opensandbox.sandbox.api.diagnostic.infrastructure.ServerError as DiagnosticServerError
import com.alibaba.opensandbox.sandbox.api.diagnostic.infrastructure.ServerException as DiagnosticServerException
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.ApiResponse as ExecdApiResponse
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.ClientError as ExecdClientError
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.ClientException as ExecdClientException
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.ServerError as ExecdServerError
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.ServerException as ExecdServerException

/**
 * Returns `true` when this throwable represents an expected "file or directory does not exist"
 * outcome rather than a genuine failure.
 *
 * Detection is intentionally restricted to the explicit [SandboxError.FILE_NOT_FOUND] server
 * error code rather than a bare HTTP 404. A 404 whose body cannot be parsed is mapped to
 * [SandboxError.UNEXPECTED_RESPONSE] and may indicate a real endpoint/routing/configuration
 * regression, which must stay loud (ERROR) instead of being silently downgraded.
 *
 * Callers (and the adapters themselves) use this to avoid treating a missing file as an error,
 * e.g. logging it at ERROR level with a full stack trace, which is just noise for a perfectly
 * normal control-flow case such as polling for a not-yet-created file.
 */
fun Throwable.isFileNotFound(): Boolean = this is SandboxApiException && error.code == SandboxError.FILE_NOT_FOUND

private fun buildSandboxApiException(
    message: String?,
    statusCode: Int,
    cause: Throwable? = null,
    errorBody: Any? = null,
    requestId: String? = null,
    retryAfter: Duration? = null,
): SandboxApiException {
    val sandboxError =
        parseSandboxError(errorBody) ?: when {
            statusCode == 429 -> SandboxError(SandboxError.RATE_LIMIT, message)
            errorBody is String -> SandboxError(UNEXPECTED_RESPONSE, errorBody)
            else -> SandboxError(UNEXPECTED_RESPONSE)
        }
    val responseBody = errorBody as? String
    val isRetryable = statusCode in RetryPolicy.DEFAULT_IDEMPOTENT_STATUS

    if (statusCode == 429) {
        return SandboxRateLimitException(
            message = message,
            statusCode = statusCode,
            cause = cause,
            error = sandboxError,
            requestId = requestId,
            retryAfter = retryAfter,
            responseBody = responseBody,
            isRetryable = isRetryable,
        )
    }

    return SandboxApiException(
        message = message,
        statusCode = statusCode,
        cause = cause,
        error = sandboxError,
        requestId = requestId,
        responseBody = responseBody,
        isRetryable = isRetryable,
    )
}

/**
 * Build the public API exception surface for handwritten OkHttp adapter paths.
 *
 * The generated clients flow through [toSandboxException]; direct OkHttp calls
 * must expose the same raw body, request metadata, rate-limit subtype, and
 * retryability signal.
 */
fun Response.toSandboxApiException(
    responseBody: String? = body?.string(),
    message: (statusCode: Int, responseBody: String?) -> String,
): SandboxApiException =
    buildSandboxApiException(
        message = message(code, responseBody),
        statusCode = code,
        errorBody = responseBody,
        requestId = header("X-Request-ID"),
        retryAfter = parseRetryAfter(header("Retry-After")),
    )

/** Build the same error surface from an execd generated-client HTTP response. */
internal fun ExecdApiResponse<*>.toSandboxApiException(message: (statusCode: Int, responseBody: String?) -> String): SandboxApiException {
    val errorBody: Any? =
        when (this) {
            is ExecdClientError<*> -> body
            is ExecdServerError<*> -> body
            else -> null
        }
    val responseBody = errorBody as? String
    return buildSandboxApiException(
        message = message(statusCode, responseBody),
        statusCode = statusCode,
        errorBody = errorBody,
        requestId = headers.extractRequestId(),
        retryAfter = headers.extractRetryAfter(),
    )
}

fun Exception.toSandboxException(): SandboxException {
    return when (this) {
        is SandboxException -> this
        is ClientException, is ServerException,
        is ExecdClientException, is ExecdServerException,
        is DiagnosticClientException, is DiagnosticServerException,
        -> this.toApiException()
        // The retry interceptor exhausted the overall deadline before the next
        // attempt: surface as a timeout, never retryable.
        is RetryDeadlineExceededException ->
            SandboxTimeoutException(
                message = "Request timed out: ${this.message}",
                cause = this,
                isRetryable = false,
            )
        // Pre-send connectivity failures: DNS, TCP connect, TLS handshake.
        is ConnectException, is UnknownHostException, is NoRouteToHostException, is SSLException ->
            SandboxConnectionException(
                message = "Network connectivity error: ${this.message}",
                cause = this,
                isRetryable = true,
            )

        // Read timeouts: the request was sent but did not finish in time.
        is SocketTimeoutException ->
            if (this.message?.lowercase()?.contains("connect") == true) {
                SandboxConnectionException(
                    message = "Network connectivity error: ${this.message}",
                    cause = this,
                    isRetryable = true,
                )
            } else {
                SandboxTimeoutException(
                    message = "Request timed out: ${this.message}",
                    cause = this,
                    isRetryable = false,
                )
            }
        is InterruptedIOException ->
            SandboxTimeoutException(
                message = "Request timed out: ${this.message}",
                cause = this,
                isRetryable = false,
            )
        is IOException ->
            SandboxInternalException(
                message = "Network connectivity error: ${this.message}",
                cause = this,
            )
        is IllegalStateException, is IllegalArgumentException ->
            SandboxInternalException(
                message = "SDK internal usage error: ${this.message}",
                cause = this,
            )
        is UnsupportedOperationException ->
            SandboxInternalException(
                message = "Operation not supported: ${this.message}",
                cause = this,
            )
        else ->
            SandboxInternalException(
                message = "Unexpected SDK error occurred: ${this.message}",
                cause = this,
            )
    }
}

private fun Exception.toApiException(): SandboxApiException {
    val (statusCode, rawResponse) =
        when (this) {
            is ClientException -> this.statusCode to this.response
            is ServerException -> this.statusCode to this.response
            is ExecdClientException -> this.statusCode to this.response
            is ExecdServerException -> this.statusCode to this.response
            is DiagnosticClientException -> this.statusCode to this.response
            is DiagnosticServerException -> this.statusCode to this.response
            else -> 0 to null
        }

    val headers: Map<String, List<String>>? =
        when (rawResponse) {
            is ClientError<*> -> rawResponse.headers
            is ServerError<*> -> rawResponse.headers
            is ExecdClientError<*> -> rawResponse.headers
            is ExecdServerError<*> -> rawResponse.headers
            is DiagnosticClientError<*> -> rawResponse.headers
            is DiagnosticServerError<*> -> rawResponse.headers
            else -> null
        }

    val requestId = headers?.extractRequestId()

    val errorBody =
        when (rawResponse) {
            is ClientError<*> -> rawResponse.body
            is ExecdServerError<*> -> rawResponse.body
            is ServerError<*> -> rawResponse.body
            is ExecdClientError<*> -> rawResponse.body
            is DiagnosticClientError<*> -> rawResponse.body
            is DiagnosticServerError<*> -> rawResponse.body
            else -> null
        }

    return buildSandboxApiException(
        message = this.message,
        statusCode = statusCode,
        cause = this,
        errorBody = errorBody,
        requestId = requestId,
        retryAfter = headers?.extractRetryAfter(),
    )
}

private fun Map<String, List<String>>.extractRequestId(): String? {
    return entries.firstOrNull { (key, _) ->
        key.equals("X-Request-ID", ignoreCase = true)
    }?.value?.firstOrNull()?.takeIf { it.isNotBlank() }
}

private fun Map<String, List<String>>.extractRetryAfter(): java.time.Duration? {
    val raw =
        entries.firstOrNull { (key, _) ->
            key.equals("Retry-After", ignoreCase = true)
        }?.value?.firstOrNull()
    return parseRetryAfter(raw)
}

fun parseSandboxError(body: Any?): SandboxError? {
    if (body == null) return null

    return runCatching {
        val jsonElement: JsonElement =
            when (body) {
                is String -> jsonParser.parseToJsonElement(body)
                else -> jsonParser.encodeToJsonElement(body)
            }

        val generic = jsonParser.decodeFromJsonElement<GenericErrorBody>(jsonElement)

        if (!generic.code.isNullOrBlank()) {
            SandboxError(code = generic.code, message = generic.message)
        } else {
            null
        }
    }.getOrNull()
}

@Serializable
private data class GenericErrorBody(
    val code: String? = null,
    val message: String? = null,
)
