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
Unified response handler for API calls.

Provides a centralized way to handle API responses, including:
1. Status code validation
2. Error response handling
3. Unified exception conversion

This eliminates the need to repeat response handling logic in each adapter method.
"""

import logging
from datetime import timedelta
from http import HTTPStatus
from typing import Any, TypeVar

from opensandbox.exceptions import (
    SandboxApiException,
    SandboxError,
    SandboxRateLimitException,
)
from opensandbox.transport._decision import parse_retry_after

logger = logging.getLogger(__name__)


T = TypeVar("T")

# Status codes the SDK treats as transient (the default idempotent
# retryable set from RetryPolicy). Surfaced via ``is_retryable`` so
# callers get a machine-checkable retry decision even after the SDK has
# exhausted its own retries.
_RETRYABLE_STATUS_CODES = frozenset(
    {
        HTTPStatus.TOO_MANY_REQUESTS,   # 429
        HTTPStatus.BAD_GATEWAY,         # 502
        HTTPStatus.SERVICE_UNAVAILABLE, # 503
    }
)


def extract_request_id(headers: Any) -> str | None:
    """
    Extract X-Request-ID from response headers in a case-insensitive way.
    """
    if not headers:
        return None
    try:
        # httpx.Headers supports case-insensitive lookup.
        value = headers.get("X-Request-ID") or headers.get("x-request-id")
        if isinstance(value, str):
            value = value.strip()
        return value or None
    except Exception:
        return None


def _status_code_to_int(status_code: Any) -> int:
    """
    Normalize status_code from openapi-python-client responses to a plain int.

    openapi-python-client may use http.HTTPStatus; some callers may already provide an int.
    """
    if isinstance(status_code, HTTPStatus):
        return int(status_code)
    if isinstance(status_code, int):
        return status_code
    value = getattr(status_code, "value", None)
    if isinstance(value, int):
        return value
    try:
        return int(status_code)
    except Exception:
        return 0


def require_parsed(response_obj: Any, expected_type: type[T], operation_name: str) -> T:
    """
    Validate and return the parsed payload from an openapi-python-client response.

    Use this after `handle_api_error()` to enforce:
    - parsed payload must exist
    - parsed payload must match the expected type
    """
    status_code = _status_code_to_int(getattr(response_obj, "status_code", 0))
    request_id = extract_request_id(getattr(response_obj, "headers", None))

    parsed = getattr(response_obj, "parsed", None)
    if parsed is None:
        raise SandboxApiException(
            message=f"{operation_name} failed: empty response",
            status_code=status_code,
            request_id=request_id,
        )
    if not isinstance(parsed, expected_type):
        raise SandboxApiException(
            message=f"{operation_name} failed: unexpected response type",
            status_code=status_code,
            request_id=request_id,
        )
    return parsed


def _retry_after(headers: Any) -> timedelta | None:
    """Return Retry-After as a timedelta from response headers, or None."""
    if not headers:
        return None
    try:
        raw = headers.get("Retry-After") or headers.get("retry-after")
    except Exception:
        return None
    return parse_retry_after(raw if isinstance(raw, str) else None)


# Upper bound on the raw response body slice we splice into an
# exception's ``str()``. The full body is always available on
# ``exc.response_body`` untruncated.
_RAW_BODY_MESSAGE_LIMIT = 512


def _raw_body_bytes(response_obj: Any) -> bytes | None:
    """Return the response's raw body as ``bytes`` when available."""
    content = getattr(response_obj, "content", None)
    if isinstance(content, bytes):
        return content
    if isinstance(content, bytearray):
        return bytes(content)
    return None


def _raw_body_message_fragment(body: bytes | None) -> str | None:
    """Best-effort decode of the raw body for splicing into an error message."""
    if not body:
        return None
    try:
        text = body.decode("utf-8", errors="replace").strip()
    except Exception:
        return None
    if not text:
        return None
    if len(text) > _RAW_BODY_MESSAGE_LIMIT:
        text = text[:_RAW_BODY_MESSAGE_LIMIT] + "…"
    return text


def build_api_exception_from_httpx(
    response: Any,
    operation_name: str = "API call",
) -> SandboxApiException:
    """
    Build a ``SandboxApiException`` (or ``SandboxRateLimitException`` on
    429) from a raw ``httpx.Response``.

    Use this from direct-httpx adapter paths (SSE bootstraps, isolated
    session endpoints) so 429 responses map to the same exception class
    as those coming through the generated openapi-python-client layer.
    """
    status_code = _status_code_to_int(getattr(response, "status_code", 0))
    headers = getattr(response, "headers", None)
    request_id = extract_request_id(headers)
    body_bytes = getattr(response, "content", None)
    if not isinstance(body_bytes, (bytes, bytearray)):
        body_bytes = None
    else:
        body_bytes = bytes(body_bytes)

    # Try to pull structured code/message from the JSON body.
    from opensandbox.adapters.converter.exception_converter import parse_sandbox_error

    sandbox_error = parse_sandbox_error(body_bytes) if body_bytes else None
    error_message = f"{operation_name} failed: HTTP {status_code}"
    if sandbox_error and sandbox_error.message:
        error_message = f"{operation_name} failed: {sandbox_error.message}"
    elif sandbox_error is None:
        raw_fragment = _raw_body_message_fragment(body_bytes)
        if raw_fragment:
            error_message = f"{error_message}: {raw_fragment}"

    is_retryable = status_code in _RETRYABLE_STATUS_CODES
    if status_code == HTTPStatus.TOO_MANY_REQUESTS:
        return SandboxRateLimitException(
            message=error_message,
            status_code=status_code,
            request_id=request_id,
            retry_after=_retry_after(headers),
            error=sandbox_error,
            response_body=body_bytes,
            is_retryable=is_retryable,
        )
    return SandboxApiException(
        message=error_message,
        status_code=status_code,
        request_id=request_id,
        error=sandbox_error,
        response_body=body_bytes,
        is_retryable=is_retryable,
    )


def handle_api_error(response_obj: Any, operation_name: str = "API call") -> None:
    """
    Check API response for errors and raise exception if needed.

    Call this before accessing response_obj.parsed to validate the response.

    Args:
        response_obj: The Response object from asyncio_detailed or sync_detailed
        operation_name: Name of the operation for error messages

    Raises:
        SandboxRateLimitException: On HTTP 429 (Too Many Requests).
        SandboxApiException: On any other HTTP >= 300.
    """
    status_code = _status_code_to_int(getattr(response_obj, "status_code", 0))
    headers = getattr(response_obj, "headers", None)
    request_id = extract_request_id(headers)
    raw_body = _raw_body_bytes(response_obj)

    logger.debug(f"{operation_name} response: status={status_code}")

    if status_code >= 300:
        error_message = f"{operation_name} failed: HTTP {status_code}"
        sandbox_error: SandboxError | None = None

        if hasattr(response_obj, "parsed") and response_obj.parsed is not None:
            parsed = response_obj.parsed
            parsed_code = getattr(parsed, "code", None)
            parsed_message = getattr(parsed, "message", None)

            if parsed_message:
                error_message = f"{operation_name} failed: {parsed_message}"
            elif parsed_code:
                error_message = f"{operation_name} failed: {parsed_code}"

            if parsed_code:
                sandbox_error = SandboxError(
                    code=str(parsed_code),
                    message=str(parsed_message or ""),
                )

        # Fall back to the raw body when the SDK could not parse a
        # structured message/code, so callers do not lose the server's
        # own explanation.
        if sandbox_error is None:
            raw_fragment = _raw_body_message_fragment(raw_body)
            if raw_fragment:
                error_message = f"{error_message}: {raw_fragment}"

        is_retryable = status_code in _RETRYABLE_STATUS_CODES
        if status_code == HTTPStatus.TOO_MANY_REQUESTS:
            raise SandboxRateLimitException(
                message=error_message,
                status_code=status_code,
                request_id=request_id,
                retry_after=_retry_after(headers),
                error=sandbox_error,
                response_body=raw_body,
                is_retryable=is_retryable,
            )

        raise SandboxApiException(
            message=error_message,
            status_code=status_code,
            request_id=request_id,
            error=sandbox_error,
            response_body=raw_body,
            is_retryable=is_retryable,
        )
