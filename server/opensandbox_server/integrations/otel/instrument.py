# Copyright 2026 Alibaba Group Holding Ltd.
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

"""Server-side instrumentation for lifecycle API handlers."""

from __future__ import annotations

import functools
import inspect
import logging
import time
from typing import Any, Callable, Optional

from fastapi import HTTPException

from opensandbox_server.integrations.otel.metrics import record_sandbox_operation
from opensandbox_server.services.constants import SandboxErrorCodes

logger = logging.getLogger(__name__)

OUTCOME_SUCCESS = "success"
OUTCOME_ERROR = "error"


def _error_code_from(exc: BaseException) -> str:
    """Pull the server error code out of a raised exception.

    Handlers raise ``HTTPException`` with ``detail={"code": ..., "message": ...}``. When the
    detail is a plain string, or the exception is not an ``HTTPException`` at all, fall back
    to the generic code so a failure is still counted rather than dropped.
    """
    if isinstance(exc, HTTPException):
        detail = exc.detail
        if isinstance(detail, dict):
            code = detail.get("code")
            if code is not None:
                return str(code)
        return f"HTTP_{exc.status_code}"
    return SandboxErrorCodes.UNKNOWN_ERROR


def instrumented_operation(operation: str) -> Callable[[Callable], Callable]:
    """Record duration and outcome of a lifecycle handler.

    Measures the server's own work at the API boundary, which is the one place where every
    runtime converges and where the error code has already been decided. Despite the 202
    status, ``POST /sandboxes`` blocks until the sandbox is provisioned — Kubernetes awaits
    ``_wait_for_sandbox_ready``, Docker awaits the provisioning thread — so ``create`` is a
    cold-start measurement rather than a scheduling one.

    ``functools.wraps`` is load-bearing: FastAPI builds the request model from the handler
    signature, and ``inspect.signature`` follows ``__wrapped__``, so the route keeps its
    parameters. Never swallows or alters an exception, and never lets a metrics failure
    affect the response.
    """

    def decorator(func: Callable) -> Callable:
        def _record(started: float, outcome: str, error_code: Optional[str]) -> None:
            # record_sandbox_operation already swallows instrument errors, but this sits on
            # every mutating route: a bug in telemetry must not be able to fail a request.
            try:
                record_sandbox_operation(
                    operation=operation,
                    duration_ms=(time.perf_counter() - started) * 1000.0,
                    outcome=outcome,
                    error_code=error_code,
                )
            except Exception:
                logger.exception("Failed to record %s operation metric", operation)

        if inspect.iscoroutinefunction(func):

            @functools.wraps(func)
            async def async_wrapper(*args: Any, **kwargs: Any) -> Any:
                started = time.perf_counter()
                try:
                    result = await func(*args, **kwargs)
                except BaseException as exc:
                    _record(started, OUTCOME_ERROR, _error_code_from(exc))
                    raise
                _record(started, OUTCOME_SUCCESS, None)
                return result

            return async_wrapper

        @functools.wraps(func)
        def wrapper(*args: Any, **kwargs: Any) -> Any:
            started = time.perf_counter()
            try:
                result = func(*args, **kwargs)
            except BaseException as exc:
                _record(started, OUTCOME_ERROR, _error_code_from(exc))
                raise
            _record(started, OUTCOME_SUCCESS, None)
            return result

        return wrapper

    return decorator
