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

"""
Authentication middleware for OpenSandbox Lifecycle API.

This module implements API Key authentication as specified in the OpenAPI spec.
API keys are configured via config.toml and validated against the OPEN-SANDBOX-API-KEY header.
"""

import re
from typing import Callable, Optional

from fastapi import Request, Response, status
from fastapi.responses import JSONResponse
from starlette.middleware.base import BaseHTTPMiddleware

from opensandbox_server.config import AppConfig, get_config

SANDBOX_API_KEY_HEADER = "OPEN-SANDBOX-API-KEY"


class AuthMiddleware(BaseHTTPMiddleware):
    """
    Middleware for API Key authentication.

    Validates the OPEN-SANDBOX-API-KEY header for all requests except health check.
    Returns 401 Unauthorized if authentication fails.
    """

    # Paths that don't require authentication
    EXEMPT_PATHS = ["/health", "/docs", "/redoc", "/openapi.json"]

    # Strict pattern for proxy-to-sandbox: /sandboxes/{id}/proxy/{port}/... with numeric port only.
    # Matches the actual route in proxy.py; rejects path traversal (..) and malformed port.
    _PROXY_PATH_RE = re.compile(r"^(/v1)?/sandboxes/[^/]+/proxy/\d+(/|$)")

    @staticmethod
    def _is_proxy_path(path: str) -> bool:
        """True only for the exact proxy-route shape; rejects path traversal (..)."""
        if ".." in path:
            return False
        return bool(AuthMiddleware._PROXY_PATH_RE.match(path))

    def __init__(self, app, config: Optional[AppConfig] = None):
        """
        Initialize authentication middleware.

        Args:
            app: FastAPI application instance
            config: Optional application configuration (for dependency injection)
        """
        super().__init__(app)
        self.config = config or get_config()
        # Read the API key directly from config; suitable for dev/test usage
        self.valid_api_keys = self._load_api_keys()

    def _load_api_keys(self) -> set:
        """
        Load valid API keys from configuration.

        Returns:
            set: Set of valid API keys
        """
        # Supports a single API key from config; extend later for secret managers
        api_key = self.config.server.api_key
        # Treat empty string as no key configured
        if api_key and api_key.strip():
            return {api_key}
        return set()

    async def dispatch(self, request: Request, call_next: Callable) -> Response:
        """
        Process each request and validate authentication.

        Args:
            request: Incoming HTTP request
            call_next: Next middleware or route handler

        Returns:
            Response: HTTP response
        """
        # Skip authentication for exempt paths
        if any(request.url.path.startswith(path) for path in self.EXEMPT_PATHS):
            return await call_next(request)

        # Skip authentication only for the exact proxy-to-sandbox route shape
        # (no path traversal, no loose substring match)
        if self._is_proxy_path(request.url.path):
            return await call_next(request)

        # If no API keys are configured, skip authentication
        if not self.valid_api_keys:
            return await call_next(request)

        # Extract API key from header
        api_key = request.headers.get(SANDBOX_API_KEY_HEADER)

        # Validate API key
        if not api_key:
            return JSONResponse(
                status_code=status.HTTP_401_UNAUTHORIZED,
                content={
                    "code": "MISSING_API_KEY",
                    "message": "Authentication credentials are missing. "
                              f"Provide API key via {SANDBOX_API_KEY_HEADER} header.",
                },
            )

        # Enforce strict comparison whenever API keys are configured
        if self.valid_api_keys and api_key not in self.valid_api_keys:
            return JSONResponse(
                status_code=status.HTTP_401_UNAUTHORIZED,
                content={
                    "code": "INVALID_API_KEY",
                    "message": "Authentication credentials are invalid. "
                              "Check your API key and try again.",
                },
            )

        # Authentication successful, proceed to next middleware/handler
        response = await call_next(request)
        return response
