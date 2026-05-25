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

"""
Request ID middleware and logging context for OpenSandbox Lifecycle API.

Reads X-Request-ID from incoming requests (or generates one), stores it in
contextvars so that all logs emitted during that request can be correlated
by request_id. Response includes X-Request-ID for client-side tracing.
"""

import logging
import uuid
from contextvars import ContextVar
from typing import Callable

from starlette.middleware.base import BaseHTTPMiddleware
from starlette.requests import Request
from starlette.responses import Response

# Context variable holding the current request ID for this async context.
# Used by RequestIdFilter to attach request_id to log records.
request_id_ctx: ContextVar[str | None] = ContextVar("request_id", default=None)

X_REQUEST_ID_HEADER = "X-Request-ID"


def get_request_id() -> str | None:
    """Return the current request ID in this async context, or None."""
    return request_id_ctx.get()


class RequestIdMiddleware(BaseHTTPMiddleware):
    """
    Middleware that sets request ID from X-Request-ID header or generates one.

    The ID is stored in a context variable so that any code (including service
    layer) running in the same request context can correlate logs via
    RequestIdFilter without passing request_id explicitly.
    """

    async def dispatch(self, request: Request, call_next: Callable) -> Response:
        raw = request.headers.get(X_REQUEST_ID_HEADER)
        request_id = (raw and raw.strip()) or uuid.uuid4().hex
        token = request_id_ctx.set(request_id)
        try:
            response = await call_next(request)
            response.headers[X_REQUEST_ID_HEADER] = request_id
            return response
        finally:
            request_id_ctx.reset(token)


class RequestIdFilter(logging.Filter):
    """
    Injects the current request_id from context into each log record.

    Attach this filter to handlers whose formatter uses %(request_id)s.
    When no request context (e.g. startup or health check), request_id is "-".
    """

    def filter(self, record: logging.LogRecord) -> bool:
        rid = get_request_id()
        setattr(record, "request_id", rid if rid else "-")
        return True
