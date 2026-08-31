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

"""ASGI middleware for low-cardinality Server HTTP request metrics."""

import logging
from time import perf_counter

from starlette.routing import Match, Router
from starlette.types import ASGIApp, Message, Receive, Scope, Send

from opensandbox_server.integrations.otel import record_http_request_duration

logger = logging.getLogger(__name__)


def _matched_route_path(scope: Scope) -> str:
    route_path = getattr(scope.get("route"), "path", None)
    if route_path:
        return route_path

    router = scope.get("router")
    if not isinstance(router, Router):
        return "unknown"

    partial_path = None
    for registered_route in router.routes:
        match, _ = registered_route.matches(scope)
        registered_path = getattr(registered_route, "path", None)
        if not registered_path:
            continue
        if match == Match.FULL:
            return registered_path
        if match == Match.PARTIAL and partial_path is None:
            partial_path = registered_path

    return partial_path or "unknown"


class HttpMetricsMiddleware:
    """Record request duration without exposing raw paths or request data."""

    def __init__(self, app: ASGIApp) -> None:
        self.app = app

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return

        started_at = perf_counter()
        status_code = 500

        async def send_wrapper(message: Message) -> None:
            nonlocal status_code
            if message["type"] == "http.response.start":
                status_code = message["status"]
            await send(message)

        try:
            await self.app(scope, receive, send_wrapper)
        except Exception:
            status_code = 500
            raise
        finally:
            try:
                route = _matched_route_path(scope)
                record_http_request_duration(
                    duration_ms=(perf_counter() - started_at) * 1000.0,
                    method=scope.get("method", "unknown"),
                    route=route,
                    status_code=status_code,
                )
            except Exception:
                logger.exception("Failed to record Server HTTP request metric")
