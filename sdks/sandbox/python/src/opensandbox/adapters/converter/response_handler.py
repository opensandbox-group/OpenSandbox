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
from http import HTTPStatus
from typing import Any, TypeVar

from opensandbox.exceptions import SandboxApiException, SandboxError

logger = logging.getLogger(__name__)


T = TypeVar("T")


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


def handle_api_error(response_obj: Any, operation_name: str = "API call") -> None:
    """
    Check API response for errors and raise exception if needed.

    Call this before accessing response_obj.parsed to validate the response.

    Args:
        response_obj: The Response object from asyncio_detailed or sync_detailed
        operation_name: Name of the operation for error messages

    Raises:
        SandboxApiException: If the response indicates an error
    """
    status_code = _status_code_to_int(getattr(response_obj, "status_code", 0))
    request_id = extract_request_id(getattr(response_obj, "headers", None))

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

        raise SandboxApiException(
            message=error_message,
            status_code=status_code,
            request_id=request_id,
            error=sandbox_error,
        )
