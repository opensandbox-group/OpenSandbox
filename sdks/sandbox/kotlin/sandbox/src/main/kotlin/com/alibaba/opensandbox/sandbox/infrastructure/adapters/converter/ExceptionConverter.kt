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
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError.Companion.UNEXPECTED_RESPONSE
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxInternalException
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.decodeFromJsonElement
import kotlinx.serialization.json.encodeToJsonElement
import java.io.IOException
import com.alibaba.opensandbox.sandbox.api.diagnostic.infrastructure.ClientError as DiagnosticClientError
import com.alibaba.opensandbox.sandbox.api.diagnostic.infrastructure.ClientException as DiagnosticClientException
import com.alibaba.opensandbox.sandbox.api.diagnostic.infrastructure.ServerError as DiagnosticServerError
import com.alibaba.opensandbox.sandbox.api.diagnostic.infrastructure.ServerException as DiagnosticServerException
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

fun Exception.toSandboxException(): SandboxException {
    return when (this) {
        is SandboxException -> this
        is ClientException, is ServerException,
        is ExecdClientException, is ExecdServerException,
        is DiagnosticClientException, is DiagnosticServerException,
        -> this.toApiException()
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

    val requestId =
        when (rawResponse) {
            is ClientError<*> -> rawResponse.headers.extractRequestId()
            is ServerError<*> -> rawResponse.headers.extractRequestId()
            is ExecdClientError<*> -> rawResponse.headers.extractRequestId()
            is ExecdServerError<*> -> rawResponse.headers.extractRequestId()
            is DiagnosticClientError<*> -> rawResponse.headers.extractRequestId()
            is DiagnosticServerError<*> -> rawResponse.headers.extractRequestId()
            else -> null
        }

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

    val sandboxError =
        parseSandboxError(errorBody) ?: if (errorBody is String) {
            SandboxError(UNEXPECTED_RESPONSE, errorBody)
        } else {
            SandboxError(UNEXPECTED_RESPONSE)
        }

    return SandboxApiException(
        message = this.message,
        statusCode = statusCode,
        cause = this,
        error = sandboxError,
        requestId = requestId,
    )
}

private fun Map<String, List<String>>.extractRequestId(): String? {
    return entries.firstOrNull { (key, _) ->
        key.equals("X-Request-ID", ignoreCase = true)
    }?.value?.firstOrNull()?.takeIf { it.isNotBlank() }
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
