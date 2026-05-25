#
# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
"""
Sandbox-related exception definitions.
"""


class SandboxError:
    """
    Defines standardized common error codes and messages for the Sandbox SDK.
    """

    INTERNAL_UNKNOWN_ERROR = "INTERNAL_UNKNOWN_ERROR"
    READY_TIMEOUT = "READY_TIMEOUT"
    UNHEALTHY = "UNHEALTHY"
    INVALID_ARGUMENT = "INVALID_ARGUMENT"
    UNEXPECTED_RESPONSE = "UNEXPECTED_RESPONSE"
    POOL_EMPTY = "POOL_EMPTY"
    POOL_ACQUIRE_FAILED = "POOL_ACQUIRE_FAILED"
    POOL_STATE_STORE_UNAVAILABLE = "POOL_STATE_STORE_UNAVAILABLE"
    POOL_STATE_STORE_CONTENTION = "POOL_STATE_STORE_CONTENTION"
    POOL_NOT_RUNNING = "POOL_NOT_RUNNING"

    def __init__(self, code: str, message: str | None = None) -> None:
        self.code = code
        self.message = message

    def __repr__(self) -> str:
        return f"SandboxError(code='{self.code}', message='{self.message}')"


class SandboxException(Exception):
    """
    Base exception class for all sandbox-related errors.

    This is the root exception class that all other sandbox exceptions inherit from.
    It provides a consistent error structure across the SDK.
    """

    def __init__(
        self,
        message: str | None = None,
        cause: Exception | None = None,
        error: SandboxError | None = None,
        request_id: str | None = None,
    ) -> None:
        super().__init__(message)
        self.__cause__ = cause
        self.error = error or SandboxError(SandboxError.INTERNAL_UNKNOWN_ERROR)
        self.request_id = request_id


class SandboxApiException(SandboxException):
    """
    Thrown when the Sandbox API returns an error response (e.g., HTTP 4xx or 5xx)
    or meets unexpected error when calling API.
    """

    def __init__(
        self,
        message: str | None = None,
        cause: Exception | None = None,
        status_code: int | None = None,
        error: SandboxError | None = None,
        request_id: str | None = None,
    ) -> None:
        super().__init__(
            message,
            cause,
            error or SandboxError(SandboxError.UNEXPECTED_RESPONSE),
            request_id=request_id,
        )
        self.status_code = status_code


class SandboxInternalException(SandboxException):
    """
    Thrown when an unexpected internal error occurs within the SDK.
    """

    def __init__(
        self,
        message: str | None = None,
        cause: Exception | None = None,
    ) -> None:
        super().__init__(
            message, cause, SandboxError(SandboxError.INTERNAL_UNKNOWN_ERROR)
        )


class SandboxUnhealthyException(SandboxException):
    """
    Thrown when the sandbox is determined to be unhealthy.
    """

    def __init__(
        self,
        message: str | None = None,
        cause: Exception | None = None,
    ) -> None:
        super().__init__(message, cause, SandboxError(SandboxError.UNHEALTHY, message))


class SandboxReadyTimeoutException(SandboxException):
    """
    Thrown when the operation times out waiting for the sandbox to become ready.
    """

    def __init__(
        self,
        message: str | None = None,
        cause: Exception | None = None,
    ) -> None:
        super().__init__(
            message, cause, SandboxError(SandboxError.READY_TIMEOUT, message)
        )


class InvalidArgumentException(SandboxException):
    """
    Thrown when an invalid argument is provided to an SDK method.
    Similar to ValueError but within the SDK's exception hierarchy.
    """

    def __init__(
        self,
        message: str | None = None,
        cause: Exception | None = None,
    ) -> None:
        super().__init__(
            message, cause, SandboxError(SandboxError.INVALID_ARGUMENT, message)
        )


class PoolEmptyException(SandboxException):
    """Thrown when FAIL_FAST acquire sees no idle sandbox."""

    def __init__(
        self,
        message: str | None = "No idle sandbox available and policy is FAIL_FAST",
        cause: Exception | None = None,
    ) -> None:
        super().__init__(message, cause, SandboxError(SandboxError.POOL_EMPTY, message))


class PoolAcquireFailedException(SandboxException):
    """Thrown when FAIL_FAST acquire sees an unusable idle sandbox candidate."""

    def __init__(
        self,
        message: str | None = "Acquire failed due to unusable idle sandbox candidate(s)",
        cause: Exception | None = None,
    ) -> None:
        super().__init__(
            message, cause, SandboxError(SandboxError.POOL_ACQUIRE_FAILED, message)
        )


class PoolStateStoreUnavailableException(SandboxException):
    """Thrown when the pool state store is unavailable."""

    def __init__(
        self,
        message: str | None = None,
        cause: Exception | None = None,
    ) -> None:
        super().__init__(
            message,
            cause,
            SandboxError(SandboxError.POOL_STATE_STORE_UNAVAILABLE, message),
        )


class PoolStateStoreContentionException(SandboxException):
    """Thrown when atomic store operations encounter contention."""

    def __init__(
        self,
        message: str | None = None,
        cause: Exception | None = None,
    ) -> None:
        super().__init__(
            message,
            cause,
            SandboxError(SandboxError.POOL_STATE_STORE_CONTENTION, message),
        )


class PoolNotRunningException(SandboxException):
    """Thrown when acquire is called while the pool is not running."""

    def __init__(
        self,
        message: str | None = "Pool is not running",
        cause: Exception | None = None,
    ) -> None:
        super().__init__(
            message, cause, SandboxError(SandboxError.POOL_NOT_RUNNING, message)
        )
