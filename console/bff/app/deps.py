# Copyright 2026 The OpenSandbox Authors
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

from __future__ import annotations

from typing import Annotated

from fastapi import Depends, HTTPException, Request, status

from app.config import Settings, get_settings
from app.session import decode_session


def get_session_payload(
    request: Request,
    settings: Annotated[Settings, Depends(get_settings)],
) -> dict:
    """Read the session only from the cookie jar.

    The cookie name is configurable, so this must not be a ``cookie`` query
    parameter. A query token would be written to access logs and could be replayed.
    """
    cookie = request.cookies.get(settings.bff_session_cookie_name)
    if not cookie:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail={"code": "UNAUTHORIZED", "message": "Session required"},
        )
    payload = decode_session(settings, cookie)
    if payload is None:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail={"code": "UNAUTHORIZED", "message": "Invalid or expired session"},
        )
    return payload


def require_tenant_session(payload: dict) -> dict:
    if payload.get("role") != "tenant":
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail={"code": "FORBIDDEN", "message": "Tenant session required"},
        )
    return payload


def require_admin_session(payload: dict) -> dict:
    if payload.get("role") != "admin":
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail={"code": "FORBIDDEN", "message": "Admin session required"},
        )
    return payload
