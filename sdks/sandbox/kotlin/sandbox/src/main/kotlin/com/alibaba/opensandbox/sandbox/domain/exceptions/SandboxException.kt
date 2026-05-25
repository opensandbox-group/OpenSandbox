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

/**
 * Base exception class for all sandbox-related errors.
 *
 * Inherits from [RuntimeException] (Unchecked Exception) to avoid forcing
 * Java callers to implement verbose try-catch blocks while still allowing
 * specific error handling when needed.
 */
open class SandboxException(
    message: String? = null,
    cause: Throwable? = null,
    val error: SandboxError,
    val requestId: String? = null,
) : RuntimeException(message, cause) {
    // Keep the old constructor signature for binary compatibility with already-compiled clients.
    constructor(
        message: String?,
        cause: Throwable?,
        error: SandboxError,
    ) : this(message = message, cause = cause, error = error, requestId = null)
}

/**
 * Thrown when the Sandbox API returns an error response (e.g., HTTP 4xx or 5xx) or meet unexpected error when calling api.
 */
class SandboxApiException(
    message: String? = null,
    cause: Throwable? = null,
    val statusCode: Int? = null,
    error: SandboxError = SandboxError(SandboxError.UNEXPECTED_RESPONSE),
    requestId: String? = null,
) : SandboxException(message, cause, error, requestId) {
    // Keep the old constructor signature for binary compatibility with already-compiled clients.
    constructor(
        message: String?,
        cause: Throwable?,
        statusCode: Int?,
        error: SandboxError,
    ) : this(message = message, cause = cause, statusCode = statusCode, error = error, requestId = null)
}

/**
 * Thrown when an unexpected internal error occurs within the SDK
 */
class SandboxInternalException(
    message: String? = null,
    cause: Throwable? = null,
) : SandboxException(
        message = message,
        cause = cause,
        error = SandboxError(SandboxError.INTERNAL_UNKNOWN_ERROR),
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
 * Thrown when atomic take or lock-update conflicts occur in the state store.
 */
class PoolStateStoreContentionException(
    message: String? = null,
    cause: Throwable? = null,
) : SandboxException(
        message = message,
        cause = cause,
        error = SandboxError(SandboxError.POOL_STATE_STORE_CONTENTION, message),
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

        /** The requested file or directory does not exist (server responds with HTTP 404). */
        const val FILE_NOT_FOUND = "FILE_NOT_FOUND"

        /** Pool-specific: no idle sandbox and policy is FAIL_FAST. */
        const val POOL_EMPTY = "POOL_EMPTY"

        /** Pool-specific: FAIL_FAST acquire failed because idle candidate(s) were unusable. */
        const val POOL_ACQUIRE_FAILED = "POOL_ACQUIRE_FAILED"

        /** Pool state store unavailable during operations. */
        const val POOL_STATE_STORE_UNAVAILABLE = "POOL_STATE_STORE_UNAVAILABLE"

        /** Pool state store contention (atomic take or lock conflicts). */
        const val POOL_STATE_STORE_CONTENTION = "POOL_STATE_STORE_CONTENTION"

        /** Pool is not in RUNNING state when acquire is requested. */
        const val POOL_NOT_RUNNING = "POOL_NOT_RUNNING"
    }
}
