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

package com.alibaba.opensandbox.sandbox.domain.exceptions

import java.time.Duration

/**
 * Base exception class for all sandbox-related errors.
 *
 * Inherits from [RuntimeException] (Unchecked Exception) to avoid forcing
 * Java callers to implement verbose try-catch blocks while still allowing
 * specific error handling when needed.
 *
 * [isRetryable] reflects whether the SDK actually decided to retry this failure:
 * budget exhaustion, deadline expiry, and caller cancellation all force it to
 * `false` even for transient causes.
 */
open class SandboxException(
    message: String? = null,
    cause: Throwable? = null,
    val error: SandboxError,
    val requestId: String? = null,
    open val isRetryable: Boolean = false,
) : RuntimeException(message, cause) {
    // Keep the old constructor signature for binary compatibility.
    constructor(
        message: String?,
        cause: Throwable?,
        error: SandboxError,
    ) : this(message = message, cause = cause, error = error, requestId = null)

    override fun toString(): String {
        val parts = mutableListOf(super.toString())
        if (!error.message.isNullOrBlank()) {
            parts += "[${error.code}] ${error.message}"
        }
        if (!requestId.isNullOrBlank()) {
            parts += "request_id=$requestId"
        }
        return parts.joinToString(" | ")
    }
}

/**
 * Thrown when the Sandbox API returns an error response (e.g., HTTP 4xx or 5xx)
 * or meets an unexpected error when calling the API.
 *
 * [responseBody] carries the raw response body from the server so callers can
 * inspect payloads the SDK could not parse into a structured [SandboxError].
 */
open class SandboxApiException(
    message: String? = null,
    cause: Throwable? = null,
    val statusCode: Int? = null,
    error: SandboxError = SandboxError(SandboxError.UNEXPECTED_RESPONSE),
    requestId: String? = null,
    val responseBody: String? = null,
    isRetryable: Boolean = false,
) : SandboxException(
        message = message,
        cause = cause,
        error = error,
        requestId = requestId,
    ) {
    @Suppress("LeakingThis")
    override val isRetryable: Boolean = isRetryable

    // Keep the old constructor signature for binary compatibility.
    @Suppress("unused", "LongLine")
    constructor(
        message: String?,
        cause: Throwable?,
        statusCode: Int?,
        error: SandboxError,
    ) : this(
        message = message,
        cause = cause,
        statusCode = statusCode,
        error = error,
        requestId = null,
        responseBody = null,
        isRetryable = false,
    )

    // Keep the five-arg signature for binary compatibility.
    @Suppress("unused", "LongLine")
    constructor(
        message: String?,
        cause: Throwable?,
        statusCode: Int?,
        error: SandboxError,
        requestId: String?,
    ) : this(
        message = message,
        cause = cause,
        statusCode = statusCode,
        error = error,
        requestId = requestId,
        responseBody = null,
        isRetryable = false,
    )
}

/**
 * Thrown when the API returns HTTP 429 (Too Many Requests).
 *
 * [retryAfter] carries the server-supplied `Retry-After` header value as a
 * [Duration] when present, so fast-fail callers can still act on it.
 */
class SandboxRateLimitException
    @JvmOverloads
    constructor(
        message: String? = null,
        cause: Throwable? = null,
        statusCode: Int? = 429,
        error: SandboxError = SandboxError(SandboxError.RATE_LIMIT, message),
        requestId: String? = null,
        val retryAfter: Duration? = null,
        responseBody: String? = null,
        isRetryable: Boolean = false,
    ) : SandboxApiException(
            message = message,
            cause = cause,
            statusCode = statusCode,
            error = error,
            requestId = requestId,
            responseBody = responseBody,
            isRetryable = isRetryable,
        )

/**
 * Thrown when an unexpected internal error occurs within the SDK
 */
open class SandboxInternalException(
    message: String? = null,
    cause: Throwable? = null,
) : SandboxException(
        message = message,
        cause = cause,
        error = SandboxError(SandboxError.INTERNAL_UNKNOWN_ERROR),
    ) {
    @Suppress("LeakingThis")
    open override val isRetryable: Boolean = false
}

/**
 * Thrown when a per-attempt timeout or overall retry deadline fires.
 *
 * Distinct from [SandboxReadyTimeoutException], which is the health-poll timeout
 * during sandbox startup.
 */
class SandboxTimeoutException(
    message: String? = null,
    cause: Throwable? = null,
    isRetryable: Boolean = false,
) : SandboxException(
        message = message,
        cause = cause,
        error = SandboxError(SandboxError.TIMEOUT, message),
        requestId = null,
        isRetryable = isRetryable,
    )

/**
 * Transport-layer failure: DNS, TCP connect, TLS, or connection reset.
 */
class SandboxConnectionException(
    message: String? = null,
    cause: Throwable? = null,
    isRetryable: Boolean = false,
) : SandboxException(
        message = message,
        cause = cause,
        error = SandboxError(SandboxError.CONNECTION, message),
        requestId = null,
        isRetryable = isRetryable,
    )

/**
 * Thrown when the operation times out waiting for the sandbox to become ready.
 */
class SandboxUnhealthyException(
    message: String? = null,
    cause: Throwable? = null,
) : SandboxException(
        message = message,
        cause = cause,
        error = SandboxError(SandboxError.UNHEALTHY, message),
    )

/**
 * Thrown when the operation times out waiting for the sandbox to become ready.
 */
class SandboxReadyTimeoutException(
    message: String? = null,
    cause: Throwable? = null,
) : SandboxException(
        message = message,
        cause = cause,
        error = SandboxError(SandboxError.READY_TIMEOUT, message),
    )

/**
 * Thrown when a snapshot reaches the `Failed` state while waiting for it to become ready.
 */
class SnapshotFailedException(
    message: String? = null,
    cause: Throwable? = null,
) : SandboxException(
        message = message,
        cause = cause,
        error = SandboxError(SandboxError.SNAPSHOT_FAILED, message),
    )

/**
 * Thrown when an invalid argument is provided to an SDK method.
 * Similar to [IllegalArgumentException] but within the SDK's exception hierarchy.
 */
class InvalidArgumentException(
    message: String? = null,
    cause: Throwable? = null,
) : SandboxException(
        message = message,
        cause = cause,
        error = SandboxError(SandboxError.INVALID_ARGUMENT, message),
    )

/**
 * Thrown when acquire is called with FAIL_FAST policy and no idle sandbox is available.
 */
class PoolEmptyException(
    message: String? = "No idle sandbox available and policy is FAIL_FAST",
    cause: Throwable? = null,
) : SandboxException(
        message = message,
        cause = cause,
        error = SandboxError(SandboxError.POOL_EMPTY, message),
    )

/**
 * Thrown when acquire cannot obtain a usable sandbox from idle candidates under FAIL_FAST policy.
 * Typical case: an idle candidate exists but connect fails (stale/unreachable).
 */
class PoolAcquireFailedException(
    message: String? = "Acquire failed due to unusable idle sandbox candidate(s)",
    cause: Throwable? = null,
) : SandboxException(
        message = message,
        cause = cause,
        error = SandboxError(SandboxError.POOL_ACQUIRE_FAILED, message),
    )

/**
 * Thrown when the pool state store is unavailable during idle take/put/lock operations.
 */
class PoolStateStoreUnavailableException(
    message: String? = null,
    cause: Throwable? = null,
) : SandboxException(
        message = message,
        cause = cause,
        error = SandboxError(SandboxError.POOL_STATE_STORE_UNAVAILABLE, message),
    )

/**
 * Thrown when acquire is called while pool is not in RUNNING state.
 */
class PoolNotRunningException(
    message: String? = "Pool is not running",
    cause: Throwable? = null,
) : SandboxException(
        message = message,
        cause = cause,
        error = SandboxError(SandboxError.POOL_NOT_RUNNING, message),
    )

/**
 * Thrown when a pool namespace is being destroyed or has already been destroyed.
 */
class PoolDestroyedException(
    message: String? = "Pool namespace is destroyed",
    cause: Throwable? = null,
) : SandboxException(
        message = message,
        cause = cause,
        error = SandboxError(SandboxError.POOL_DESTROYED, message),
    )

/**
 * Thrown when a pool destroy operation has started but did not complete. The pool namespace
 * remains fenced in DESTROYING state so callers can retry destroy instead of silently resuming
 * a partially-cleaned pool.
 */
class PoolDestroyIncompleteException(
    message: String? = "Pool destroy did not complete",
    cause: Throwable? = null,
) : SandboxException(
        message = message,
        cause = cause,
        error = SandboxError(SandboxError.POOL_DESTROY_INCOMPLETE, message),
    )

/**
 * Defines standardized common error codes and messages for the Sandbox SDK.
 */
data class SandboxError(
    val code: String,
    val message: String? = null,
) {
    companion object {
        const val INTERNAL_UNKNOWN_ERROR = "INTERNAL_UNKNOWN_ERROR"
        const val READY_TIMEOUT = "READY_TIMEOUT"
        const val UNHEALTHY = "UNHEALTHY"
        const val INVALID_ARGUMENT = "INVALID_ARGUMENT"
        const val UNEXPECTED_RESPONSE = "UNEXPECTED_RESPONSE"

        /** A per-attempt timeout or overall retry deadline fired. */
        const val TIMEOUT = "TIMEOUT"

        /** Transport-layer failure: DNS, TCP connect, TLS, or connection reset. */
        const val CONNECTION = "CONNECTION"

        /** The API returned HTTP 429 (Too Many Requests). */
        const val RATE_LIMIT = "RATE_LIMIT"

        /** A snapshot reached the `Failed` state while waiting for it to become ready. */
        const val SNAPSHOT_FAILED = "SNAPSHOT_FAILED"

        /** The requested file or directory does not exist (server responds with HTTP 404). */
        const val FILE_NOT_FOUND = "FILE_NOT_FOUND"

        /** Pool-specific: no idle sandbox and policy is FAIL_FAST. */
        const val POOL_EMPTY = "POOL_EMPTY"

        /** Pool-specific: FAIL_FAST acquire failed because idle candidate(s) were unusable. */
        const val POOL_ACQUIRE_FAILED = "POOL_ACQUIRE_FAILED"

        /** Pool state store unavailable during operations. */
        const val POOL_STATE_STORE_UNAVAILABLE = "POOL_STATE_STORE_UNAVAILABLE"

        /** Pool is not in RUNNING state when acquire is requested. */
        const val POOL_NOT_RUNNING = "POOL_NOT_RUNNING"

        /** Pool namespace is destroying or destroyed. */
        const val POOL_DESTROYED = "POOL_DESTROYED"

        /** Pool destroy started but did not complete. */
        const val POOL_DESTROY_INCOMPLETE = "POOL_DESTROY_INCOMPLETE"
    }
}
