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

from fastapi import APIRouter, HTTPException, WebSocket

from app.config import get_settings
from app.routes.session_keys import api_key_for_tenant_name, tenant_api_key
from app.session import decode_session
from app.shell import serve_shell

router = APIRouter(tags=["shell"])


async def _reject(websocket: WebSocket, code: str, message: str, close_code: int) -> None:
    await websocket.accept()
    await websocket.send_json({"type": "error", "code": code, "error": message})
    await websocket.close(code=close_code)


def _session_payload(websocket: WebSocket) -> dict | None:
    settings = get_settings()
    cookie = websocket.cookies.get(settings.bff_session_cookie_name)
    if not cookie:
        return None
    return decode_session(settings, cookie)


def _http_error(exc: HTTPException) -> tuple[str, str]:
    detail = exc.detail if isinstance(exc.detail, dict) else {}
    return str(detail.get("code") or "ERROR"), str(detail.get("message") or "Request failed")


@router.websocket("/sandboxes/{sandbox_id}/shell/ws")
async def tenant_shell(websocket: WebSocket, sandbox_id: str) -> None:
    payload = _session_payload(websocket)
    if payload is None:
        await _reject(websocket, "UNAUTHORIZED", "Session required", 4401)
        return
    if payload.get("role") != "tenant":
        await _reject(websocket, "FORBIDDEN", "Tenant session required", 4403)
        return
    try:
        api_key = tenant_api_key(payload)
    except HTTPException as exc:
        code, message = _http_error(exc)
        await _reject(websocket, code, message, 4401)
        return
    await serve_shell(websocket, api_key=api_key, sandbox_id=sandbox_id)


@router.websocket("/admin/sandboxes/{sandbox_id}/shell/ws")
async def admin_shell(websocket: WebSocket, sandbox_id: str) -> None:
    payload = _session_payload(websocket)
    if payload is None:
        await _reject(websocket, "UNAUTHORIZED", "Session required", 4401)
        return
    if payload.get("role") != "admin":
        await _reject(websocket, "FORBIDDEN", "Admin session required", 4403)
        return
    tenant = websocket.query_params.get("tenant")
    if not tenant:
        await _reject(websocket, "TENANT_REQUIRED", "Admin shell requires tenant", 4400)
        return
    try:
        api_key = api_key_for_tenant_name(tenant)
    except HTTPException as exc:
        code, message = _http_error(exc)
        await _reject(websocket, code, message, 4400)
        return
    await serve_shell(websocket, api_key=api_key, sandbox_id=sandbox_id)
