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

"""Server-side instrumentation for lifecycle API routes."""

from __future__ import annotations

import logging
import time
from typing import Callable, Optional

from fastapi import HTTPException
from fastapi.exceptions import RequestValidationError
from fastapi.routing import APIRoute
from starlette.requests import Request
from starlette.responses import Response

from opensandbox_server.integrations.otel.metrics import record_sandbox_operation
from opensandbox_server.services.constants import SandboxErrorCodes

logger = logging.getLogger(__name__)

OUTCOME_SUCCESS = "success"
OUTCOME_ERROR = "error"

# Set by lifecycle_operation, read by InstrumentedRoute.
_OPERATION_ATTRIBUTE = "__opensandbox_operation__"


def lifecycle_operation(operation: str) -> Callable[[Callable], Callable]:
    """Mark a route handler as a lifecycle operation, for ``InstrumentedRoute`` to record.

    A marker rather than a wrapper: recording happens in the route class, which also sees
    requests rejected before the handler runs. Wrapping here as well would double-count, and
    would put a layer between FastAPI and the handler signature for no benefit.
    """

    def decorator(func: Callable) -> Callable:
        setattr(func, _OPERATION_ATTRIBUTE, operation)
        return func

    return decorator


def _error_code_from(exc: BaseException) -> str:
    """Pull a bounded error code out of a failure.

    Handlers raise ``HTTPException`` with ``detail={"code": ..., "message": ...}``. Request
    validation fails before any handler runs and carries no server code, so it is reported by
    status. Anything unexpected gets the generic code, so a failure is still counted.
    """
    if isinstance(exc, RequestValidationError):
        return "HTTP_422"
    if isinstance(exc, HTTPException):
        detail = exc.detail
        if isinstance(detail, dict):
            code = detail.get("code")
            if code is not None:
                return str(code)
        return f"HTTP_{exc.status_code}"
    return SandboxErrorCodes.UNKNOWN_ERROR


def _record(operation: str, started: float, outcome: str, error_code: Optional[str]) -> None:
    """Record one sample, swallowing anything that goes wrong.

    This sits on every mutating route, so a bug in telemetry must not be able to fail a
    request. ``record_sandbox_operation`` already guards its instruments; this guards the rest.
    """
    try:
        record_sandbox_operation(
            operation=operation,
            duration_ms=(time.perf_counter() - started) * 1000.0,
            outcome=outcome,
            error_code=error_code,
        )
    except Exception:
        logger.exception("Failed to record %s operation metric", operation)


class InstrumentedRoute(APIRoute):
    """Records duration and outcome for routes marked with ``lifecycle_operation``.

    Instrumenting the route rather than the handler covers the whole API boundary: FastAPI
    raises ``RequestValidationError`` before the endpoint is called, so a decorator on the
    handler never sees a malformed request and an error-rate dashboard would undercount it.

    Unmarked routes — the read-only endpoints — get the untouched handler, so nothing is paid
    for them.

    Note ``POST /sandboxes`` answers 202 but blocks until the sandbox is provisioned
    (Kubernetes awaits pod readiness, Docker awaits the provisioning thread), so ``create``
    measures cold start rather than scheduling.
    """

    def get_route_handler(self) -> Callable:
        handler = super().get_route_handler()
        operation: Optional[str] = getattr(self.endpoint, _OPERATION_ATTRIBUTE, None)
        if operation is None:
            return handler

        async def instrumented_handler(request: Request) -> Response:
            started = time.perf_counter()
            try:
                response = await handler(request)
            except BaseException as exc:
                _record(operation, started, OUTCOME_ERROR, _error_code_from(exc))
                raise
            # A handler that returns an error status instead of raising still failed.
            if response.status_code >= 400:
                _record(operation, started, OUTCOME_ERROR, f"HTTP_{response.status_code}")
            else:
                _record(operation, started, OUTCOME_SUCCESS, None)
            return response

        return instrumented_handler


__all__ = ["InstrumentedRoute", "lifecycle_operation"]
