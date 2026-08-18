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
from typing import Any

from starlette.types import ASGIApp, Message, Receive, Scope, Send

from opensandbox_server.integrations.otel.metrics import record_http_request_duration

_UNKNOWN_ROUTE = "unknown"


class HttpRequestMetricsMiddleware:
    """
    ASGI middleware that records ``server.http.request.duration`` for every request.

    Uses the matched route template (``scope["route"].path``) so the histogram
    stays low-cardinality, falling back to ``unknown`` when no route matched
    (e.g. authentication short-circuits before routing, or a 404). Status codes
    are captured for successful responses, auth failures, validation errors, and
    unhandled exceptions (recorded as 500).

    The downstream ASGI app is awaited to completion, so streaming responses
    (e.g. the lifecycle proxy) are measured for their full duration rather than
    just time-to-headers.
    """

    def __init__(self, app: ASGIApp) -> None:
        self.app = app

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return

        start = time.perf_counter()
        status_code = 500

        async def send_wrapper(message: Message) -> None:
            nonlocal status_code
            if message["type"] == "http.response.start":
                status_code = message["status"]
            await send(message)

        try:
            await self.app(scope, receive, send_wrapper)
        finally:
            duration_ms = (time.perf_counter() - start) * 1000.0
            route: Any = scope.get("route")
            route_path = getattr(route, "path", None) or _UNKNOWN_ROUTE
            record_http_request_duration(
                duration_ms=duration_ms,
                http_method=(scope.get("method") or "UNKNOWN").upper(),
                http_route=route_path,
                http_status_code=status_code,
            )
