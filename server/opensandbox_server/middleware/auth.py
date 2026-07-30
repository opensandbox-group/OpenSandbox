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

Supports two modes:
- Single-tenant: validates against server.api_key (legacy)
- Multi-tenant: delegates to a TenantProvider for key→tenant resolution
"""

import logging
import re
from typing import Callable, Optional

from fastapi import Request, Response, status
from fastapi.responses import JSONResponse
from starlette.middleware.base import BaseHTTPMiddleware

from opensandbox_server.config import AppConfig, get_config
from opensandbox_server.tenants.context import set_current_tenant
from opensandbox_server.tenants.provider import TenantProvider, TenantProviderUnavailable

logger = logging.getLogger(__name__)

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

    def __init__(self, app, config: Optional[AppConfig] = None, tenant_provider: Optional[TenantProvider] = None):
        super().__init__(app)
        self.config = config or get_config()
        self.tenant_provider = tenant_provider
        self.valid_api_keys = self._load_api_keys()

    def _load_api_keys(self) -> set:
        api_key = self.config.server.api_key
        if api_key and api_key.strip():
            return {api_key}
        return set()

    @property
    def _is_multi_tenant(self) -> bool:
        return self.tenant_provider is not None

    async def dispatch(self, request: Request, call_next: Callable) -> Response:
        if any(request.url.path.startswith(path) for path in self.EXEMPT_PATHS):
            return await call_next(request)

        if self._is_proxy_path(request.url.path) and not self._is_multi_tenant:
            return await call_next(request)

        # If no API keys configured AND no tenant provider → skip auth
        if not self._is_multi_tenant and not self.valid_api_keys:
            return await call_next(request)

        api_key = request.headers.get(SANDBOX_API_KEY_HEADER)

        if not api_key:
            return JSONResponse(
                status_code=status.HTTP_401_UNAUTHORIZED,
                content={
                    "code": "MISSING_API_KEY",
                    "message": "Authentication credentials are missing. "
                              f"Provide API key via {SANDBOX_API_KEY_HEADER} header.",
                },
            )

        if self._is_multi_tenant:
            return await self._authenticate_multi_tenant(api_key, request, call_next)
        else:
            return await self._authenticate_single_tenant(api_key, request, call_next)

    async def _authenticate_multi_tenant(
        self, api_key: str, request: Request, call_next: Callable
    ) -> Response:
        import asyncio

        try:
            tenant = await asyncio.to_thread(self.tenant_provider.lookup, api_key)
        except TenantProviderUnavailable as e:
            logger.error("Tenant provider unavailable: %s", e)
            return JSONResponse(
                status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
                content={
                    "code": "TENANT_PROVIDER_UNAVAILABLE",
                    "message": "Tenant authentication service is temporarily unavailable.",
                },
            )

        if tenant is None:
            return JSONResponse(
                status_code=status.HTTP_401_UNAUTHORIZED,
                content={
                    "code": "INVALID_API_KEY",
                    "message": "Authentication credentials are invalid. "
                              "Check your API key and try again.",
                },
            )

        set_current_tenant(tenant)
        request.state.tenant = tenant
        response = await call_next(request)
        return response

    async def _authenticate_single_tenant(
        self, api_key: str, request: Request, call_next: Callable
    ) -> Response:
        if self.valid_api_keys and api_key not in self.valid_api_keys:
            return JSONResponse(
                status_code=status.HTTP_401_UNAUTHORIZED,
                content={
                    "code": "INVALID_API_KEY",
                    "message": "Authentication credentials are invalid. "
                              "Check your API key and try again.",
                },
            )

        set_current_tenant(None)
        response = await call_next(request)
        return response
