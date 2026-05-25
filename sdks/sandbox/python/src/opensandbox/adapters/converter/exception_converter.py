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
Exception converter utilities.

Provides conversion functions from API exceptions to domain exceptions,
similar to the Kotlin SDK ExceptionConverter pattern.

This module handles:
1. Converting openapi-python-client generated exceptions
2. Converting httpx HTTP errors
3. Converting network/IO errors
4. Parsing error response bodies to extract SandboxError information
"""

import json
import logging
from typing import Any

from httpx import (
    ConnectError,
    HTTPStatusError,
    NetworkError,
    ReadTimeout,
    TimeoutException,
    WriteTimeout,
)

from opensandbox.api.execd.errors import UnexpectedStatus as ExecdUnexpectedStatus
from opensandbox.api.lifecycle.errors import (
    UnexpectedStatus as LifecycleUnexpectedStatus,
)
from opensandbox.exceptions import (
    InvalidArgumentException,
    SandboxApiException,
    SandboxError,
    SandboxException,
    SandboxInternalException,
)

logger = logging.getLogger(__name__)

UNEXPECTED_STATUS_TYPES = (LifecycleUnexpectedStatus, ExecdUnexpectedStatus)
HTTPX_NETWORK_ERROR_TYPES = (
    ConnectError,
    TimeoutException,
    NetworkError,
    ReadTimeout,
    WriteTimeout,
)


class ExceptionConverter:
    """
    Exception converter utilities following Kotlin SDK patterns.

    Provides static methods to convert various exceptions to sandbox exceptions,
    including proper parsing of error response bodies.
    """

    @staticmethod
    def to_sandbox_exception(e: Exception) -> SandboxException:
        """
        Convert any exception to a SandboxException.

        Following Kotlin SDK pattern:
        - SandboxException -> return as-is
        - API client exceptions -> convert to SandboxApiException
        - IOError/network errors -> convert to SandboxInternalException with network message
        - IllegalArgumentError/ValueError -> convert to SandboxInternalException with usage message
        - Other exceptions -> convert to SandboxInternalException with unexpected error message

        Args:
            e: The original exception

        Returns:
            A SandboxException subclass
        """
        # If already a SandboxException, return as-is
        if isinstance(e, SandboxException):
            return e

        # Handle openapi-python-client UnexpectedStatus error
        if _is_unexpected_status_error(e):
            return _convert_unexpected_status_to_api_exception(e)

        # Handle httpx HTTPStatusError
        if _is_httpx_status_error(e):
            return _convert_httpx_error_to_api_exception(e)

        # Handle network/IO errors
        if isinstance(e, (IOError, OSError, ConnectionError)):
            return SandboxInternalException(
                message=f"Network connectivity error: {e}",
                cause=e,
            )

        # Handle httpx network errors
        if _is_httpx_network_error(e):
            return SandboxInternalException(
                message=f"Network connectivity error: {e}",
                cause=e,
            )

        # Handle validation and argument errors (SDK usage errors)
        # - ValueError/TypeError are typically raised for invalid user inputs or model validation
        # - Pydantic ValidationError represents invalid input data for SDK models
        try:
            from pydantic import ValidationError  # type: ignore

            if isinstance(e, ValidationError):
                return InvalidArgumentException(message=str(e), cause=e)
        except Exception:
            # If pydantic isn't available for some reason, just ignore and continue
            pass

        if isinstance(e, (ValueError, TypeError)):
            return InvalidArgumentException(message=str(e), cause=e)

        # Handle unsupported operations
        if isinstance(e, NotImplementedError):
            return SandboxInternalException(
                message=f"Operation not supported: {e}",
                cause=e,
            )

        # Default to unexpected error
        return SandboxInternalException(
            message=f"Unexpected SDK error occurred: {e}",
            cause=e,
        )


def _is_unexpected_status_error(e: Exception) -> bool:
    """Check if exception is an openapi-python-client UnexpectedStatus error."""
    return isinstance(e, UNEXPECTED_STATUS_TYPES)


def _is_httpx_status_error(e: Exception) -> bool:
    """Check if exception is an httpx HTTPStatusError."""
    return isinstance(e, HTTPStatusError)


def _is_httpx_network_error(e: Exception) -> bool:
    """Check if exception is an httpx network-related error."""
    return isinstance(e, HTTPX_NETWORK_ERROR_TYPES)


def _convert_unexpected_status_to_api_exception(e: Exception) -> SandboxApiException:
    """Convert openapi-python-client UnexpectedStatus to SandboxApiException."""
    status_code = getattr(e, "status_code", 0)
    content = getattr(e, "content", b"")

    # Try to parse error body
    sandbox_error = _parse_error_body(content)

    return SandboxApiException(
        message=f"API error: HTTP {status_code}",
        status_code=status_code,
        cause=e,
        error=sandbox_error,
    )


def _convert_httpx_error_to_api_exception(e: Exception) -> SandboxApiException:
    """Convert httpx HTTPStatusError to SandboxApiException."""
    response = getattr(e, "response", None)
    status_code = response.status_code if response else 0
    content = response.content if response else b""
    request_id = None
    if response is not None:
        from opensandbox.adapters.converter.response_handler import extract_request_id

        request_id = extract_request_id(response.headers)

    # Try to parse error body
    sandbox_error = _parse_error_body(content)

    return SandboxApiException(
        message=f"API error: HTTP {status_code}",
        status_code=status_code,
        cause=e,
        error=sandbox_error,
        request_id=request_id,
    )


def _parse_error_body(body: Any) -> SandboxError | None:
    """
    Parse error body to extract SandboxError information.

    Similar to Kotlin SDK's parseSandboxError function.

    Args:
        body: The error response body (bytes, str, or dict)

    Returns:
        SandboxError if parsing succeeds, None otherwise
    """
    if body is None:
        return None

    try:
        # Convert bytes to string
        if isinstance(body, bytes):
            if not body:
                return None
            body = body.decode("utf-8", errors="replace")

        if isinstance(body, str) and not body:
            return None

        # Parse JSON string
        if isinstance(body, str):
            try:
                body = json.loads(body)
            except json.JSONDecodeError:
                # If not JSON, return error with the raw string as message
                return SandboxError(
                    code=SandboxError.UNEXPECTED_RESPONSE,
                    message=body,
                )

        # FastAPI HTTPException bodies are commonly wrapped as {"detail": {"code": ..., "message": ...}}.
        if isinstance(body, dict):
            if isinstance(body.get("detail"), dict):
                body = body["detail"]

            code: str | None = body.get("code")
            message: str | None = body.get("message")

            if code:
                return SandboxError(code=code, message=message or "")

        return None

    except Exception as ex:
        logger.debug("Failed to parse error body: %s", ex)
        return None


def parse_sandbox_error(body: Any) -> SandboxError | None:
    """
    Public function to parse error body to SandboxError.

    Exposed for use by other modules that need to parse error bodies.
    """
    return _parse_error_body(body)
