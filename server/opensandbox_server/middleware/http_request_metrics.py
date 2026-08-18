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
HTTP request duration metrics middleware.

Records a low-cardinality histogram (``server.http.request.duration``) for every
request handled by the lifecycle Server, using the matched route template
instead of the raw URL path. Recording is a no-op when OTEL metrics are disabled.
"""

import time
from typing import Callable

from starlette.middleware.base import BaseHTTPMiddleware
from starlette.requests import Request
from starlette.responses import Response

from opensandbox_server.integrations.otel.metrics import record_http_request_duration

_UNKNOWN_ROUTE = "unknown"


class HttpRequestMetricsMiddleware(BaseHTTPMiddleware):
    """
    Middleware that records ``server.http.request.duration`` for every request.

    Uses the matched route template (``request.scope["route"].path``) so the
    histogram stays low-cardinality, falling back to ``unknown`` when no route
    matched (e.g. unmatched paths). Status codes are captured for successful
    responses, authentication failures, validation errors, and unhandled
    exceptions (surfaced as 500 responses by Starlette).
    """

    async def dispatch(self, request: Request, call_next: Callable) -> Response:
        start = time.perf_counter()
        response: Response | None = None
        try:
            response = await call_next(request)
            return response
        finally:
            duration_ms = (time.perf_counter() - start) * 1000.0
            route = request.scope.get("route")
            route_path = getattr(route, "path", None) or _UNKNOWN_ROUTE
            record_http_request_duration(
                duration_ms=duration_ms,
                http_method=(request.method or "UNKNOWN").upper(),
                http_route=route_path,
                http_status_code=getattr(response, "status_code", 500),
            )
