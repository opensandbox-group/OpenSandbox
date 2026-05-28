# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.

"""
Authentication middleware: API key path (legacy) and optional user identity (OSEP-0006).
"""

import re
from typing import Callable, Optional

from fastapi import Request, Response, status
from fastapi.responses import JSONResponse
from starlette.middleware.base import BaseHTTPMiddleware

from opensandbox_server.config import (
    AUTH_MODE_API_KEY_AND_USER,
    AUTH_MODE_API_KEY_ONLY,
    USER_MODE_TRUSTED_HEADER,
    AppConfig,
    get_config,
)
from opensandbox_server.middleware.principal import build_user_principal, principal_for_api_key

class AuthMiddleware(BaseHTTPMiddleware):
    """
    Validates ``OPEN-SANDBOX-API-KEY`` when configured, with optional dual auth for the console.
    """

    API_KEY_HEADER = "OPEN-SANDBOX-API-KEY"

    EXEMPT_PATHS = ["/health", "/docs", "/redoc", "/openapi.json"]
    _PROXY_PATH_RE = re.compile(r"^(/v1)?/sandboxes/[^/]+/proxy/\d+(/|$)")

    @staticmethod
    def _is_proxy_path(path: str) -> bool:
        if ".." in path:
            return False
        return bool(AuthMiddleware._PROXY_PATH_RE.match(path))

    # API route prefixes that must never be bypassed by the console auth skip.
    _PROTECTED_API_PREFIXES = (
        "/v1",
        "/auth",
        "/sandboxes",
        "/snapshots",
        "/pools",
        "/devops",
    )

    @staticmethod
    def _is_console_path(path: str, mount: str) -> bool:
        if ".." in path:
            return False
        base = mount.rstrip("/") or "/console"
        # Reject any mount that collides with known API route prefixes so a
        # misconfigured mount_path (e.g. "/auth") cannot bypass authentication.
        if any(
            base == p or base.startswith(p + "/")
            for p in AuthMiddleware._PROTECTED_API_PREFIXES
        ):
            return False
        return path == base or path.startswith(base + "/")

    def __init__(self, app, config: Optional[AppConfig] = None):
        super().__init__(app)
        self.config = config or get_config()
        self.valid_api_keys = self._load_api_keys()

    def _load_api_keys(self) -> set:
        api_key = self.config.server.api_key
        if api_key and api_key.strip():
            return {api_key}
        return set()

    def _try_trusted_user_principal(self, request: Request):
        if self.config.auth.user_mode != USER_MODE_TRUSTED_HEADER:
            return None
        th = self.config.auth.trusted_header
        raw_user = request.headers.get(th.user_header)
        if raw_user is None or not str(raw_user).strip():
            return None
        raw_team = request.headers.get(th.team_header)
        roles = request.headers.get(th.roles_header)
        try:
            return build_user_principal(
                str(raw_user).strip(),
                str(raw_team).strip() if raw_team is not None else None,
                roles,
                self.config.authz,
            )
        except ValueError:
            return None

    async def dispatch(self, request: Request, call_next: Callable) -> Response:
        if any(request.url.path.startswith(path) for path in self.EXEMPT_PATHS):
            return await call_next(request)

        if self._is_proxy_path(request.url.path):
            return await call_next(request)

        if self.config.console.enabled and self._is_console_path(request.url.path, self.config.console.mount_path):
            return await call_next(request)

        mode = self.config.auth.mode
        has_keys = bool(self.valid_api_keys)

        # No API keys: open access (legacy) OR require user headers for api_key_and_user
        if not has_keys:
            if mode == AUTH_MODE_API_KEY_AND_USER:
                principal = self._try_trusted_user_principal(request)
                if principal is None:
                    return JSONResponse(
                        status_code=status.HTTP_401_UNAUTHORIZED,
                        content={
                            "code": "MISSING_TRUSTED_IDENTITY",
                            "message": "User authentication requires trusted identity headers (e.g. "
                            f"{self.config.auth.trusted_header.user_header}).",
                        },
                    )
                request.state.principal = principal
                return await call_next(request)
            return await call_next(request)

        api_key = request.headers.get(self.API_KEY_HEADER)
        if api_key:
            if api_key in self.valid_api_keys:
                request.state.principal = principal_for_api_key()
                return await call_next(request)
            return JSONResponse(
                status_code=status.HTTP_401_UNAUTHORIZED,
                content={
                    "code": "INVALID_API_KEY",
                    "message": "Authentication credentials are invalid. "
                    "Check your API key and try again.",
                },
            )

        if mode == AUTH_MODE_API_KEY_ONLY:
            return JSONResponse(
                status_code=status.HTTP_401_UNAUTHORIZED,
                content={
                    "code": "MISSING_API_KEY",
                    "message": "Authentication credentials are missing. "
                    "Provide API key via OPEN-SANDBOX-API-KEY header.",
                },
            )

        principal = self._try_trusted_user_principal(request)
        if principal is None:
            return JSONResponse(
                status_code=status.HTTP_401_UNAUTHORIZED,
                content={
                    "code": "MISSING_TRUSTED_IDENTITY",
                    "message": "User authentication requires trusted identity headers (e.g. "
                    f"{self.config.auth.trusted_header.user_header}).",
                },
            )
        request.state.principal = principal
        return await call_next(request)


SANDBOX_API_KEY_HEADER = AuthMiddleware.API_KEY_HEADER
