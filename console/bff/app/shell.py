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

"""Bridge a browser WebSocket to an execd PTY without changing the lifecycle server."""

from __future__ import annotations

import asyncio
import inspect
import logging
import re
from urllib.parse import quote

import httpx
import websockets
from fastapi import WebSocket, WebSocketDisconnect

from app.config import Settings, get_settings
from app.lifecycle import LifecycleClient

logger = logging.getLogger(__name__)

EXECD_PORT = 44772
_SANDBOX_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
_SESSION_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")


class ShellError(Exception):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


def shell_endpoint_mode(settings: Settings) -> str:
    mode = settings.bff_shell_endpoint_mode.strip().lower() or "server"
    if mode not in {"server", "gateway"}:
        raise ShellError(
            "SHELL_ENDPOINT_MODE",
            "BFF_SHELL_ENDPOINT_MODE must be server or gateway",
        )
    return mode


def server_proxy_endpoint(settings: Settings, sandbox_id: str) -> str:
    base = settings.lifecycle_api_base.strip().rstrip("/")
    return f"{base}/sandboxes/{quote(sandbox_id, safe='')}/proxy/{EXECD_PORT}"


def execd_protocol(settings: Settings) -> str:
    configured = settings.bff_execd_protocol.strip().lower()
    if configured in {"http", "https"}:
        return configured
    if settings.lifecycle_api_base.startswith("https://"):
        return "https"
    return "http"


def execd_http_base(endpoint: str, protocol: str) -> str:
    trimmed = endpoint.strip().rstrip("/")
    if trimmed.startswith("https://") or trimmed.startswith("http://"):
        return trimmed
    scheme = "https" if protocol == "https" else "http"
    return f"{scheme}://{trimmed}"


def execd_ws_url(http_base: str, session_id: str) -> str:
    if http_base.startswith("https://"):
        ws_base = "wss://" + http_base[len("https://") :]
    elif http_base.startswith("http://"):
        ws_base = "ws://" + http_base[len("http://") :]
    else:
        raise ShellError("ENDPOINT_UNAVAILABLE", "Execd endpoint is missing a scheme")
    return f"{ws_base}/pty/{quote(session_id, safe='')}/ws"


def is_server_proxy_endpoint(endpoint: str) -> bool:
    return "/sandboxes/" in endpoint and "/proxy/" in endpoint


def execd_request_headers(
    endpoint: str,
    endpoint_headers: dict[str, object],
    api_key: str,
) -> dict[str, str]:
    headers = {
        str(key): str(value)
        for key, value in endpoint_headers.items()
        if value is not None
    }
    if is_server_proxy_endpoint(endpoint) and api_key:
        headers["OPEN-SANDBOX-API-KEY"] = api_key
    return headers


def connect_execd(url: str, headers: dict[str, str]):
    timeout = get_settings().bff_http_timeout_seconds
    parameters = inspect.signature(websockets.connect).parameters
    header_arg = "additional_headers" if "additional_headers" in parameters else "extra_headers"
    kwargs = {
        header_arg: headers,
        "open_timeout": timeout,
        "max_size": 8 * 1024 * 1024,
    }
    return websockets.connect(url, **kwargs)


async def create_pty_session(http_base: str, headers: dict[str, str]) -> str:
    timeout = get_settings().bff_http_timeout_seconds
    async with httpx.AsyncClient(timeout=timeout) as client:
        response = await client.post(f"{http_base}/pty", headers=headers, json={})
    if response.status_code >= 400:
        raise ShellError("PTY_CREATE_FAILED", f"execd rejected the shell session ({response.status_code})")
    payload = response.json()
    session_id = payload.get("session_id") if isinstance(payload, dict) else None
    if not isinstance(session_id, str) or not _SESSION_ID.match(session_id):
        raise ShellError("PTY_CREATE_FAILED", "execd did not return a session id")
    return session_id


async def delete_pty_session(http_base: str, session_id: str, headers: dict[str, str]) -> None:
    timeout = get_settings().bff_http_timeout_seconds
    try:
        async with httpx.AsyncClient(timeout=timeout) as client:
            await client.delete(f"{http_base}/pty/{quote(session_id, safe='')}", headers=headers)
    except Exception:
        logger.warning("failed to delete pty session %s", session_id, exc_info=True)


def _upstream_message(response: httpx.Response) -> str:
    try:
        payload = response.json()
    except Exception:
        payload = None
    if isinstance(payload, dict):
        detail = payload.get("detail")
        if isinstance(detail, dict) and detail.get("message"):
            return str(detail["message"])
        if payload.get("message"):
            return str(payload["message"])
    return f"Lifecycle endpoint lookup failed ({response.status_code})"


async def _send_error(websocket: WebSocket, code: str, message: str) -> None:
    try:
        await websocket.send_json({"type": "error", "code": code, "error": message})
    except Exception:
        return


async def bridge_pty(browser: WebSocket, execd_ws) -> None:
    async def browser_to_execd() -> None:
        while True:
            try:
                message = await browser.receive()
            except WebSocketDisconnect:
                return
            if message["type"] == "websocket.disconnect":
                return
            text = message.get("text")
            data = message.get("bytes")
            if text is not None:
                await execd_ws.send(text)
            elif data is not None:
                await execd_ws.send(data)

    async def execd_to_browser() -> None:
        async for payload in execd_ws:
            if isinstance(payload, str):
                await browser.send_text(payload)
            else:
                await browser.send_bytes(payload)

    tasks = {
        asyncio.create_task(browser_to_execd()),
        asyncio.create_task(execd_to_browser()),
    }
    _done, pending = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
    for task in pending:
        task.cancel()
    await asyncio.gather(*pending, return_exceptions=True)
    for task in tasks:
        if task.cancelled():
            continue
        exc = task.exception()
        if exc is not None:
            raise exc


async def serve_shell(websocket: WebSocket, *, api_key: str, sandbox_id: str) -> None:
    await websocket.accept()
    session_id: str | None = None
    http_base: str | None = None
    headers: dict[str, str] = {}
    try:
        if not _SANDBOX_ID.match(sandbox_id):
            raise ShellError("INVALID_SANDBOX", "Invalid sandbox id")
        settings = get_settings()
        lifecycle = LifecycleClient(settings)
        response = await lifecycle.request(
            api_key,
            "GET",
            f"/sandboxes/{quote(sandbox_id, safe='')}/endpoints/{EXECD_PORT}",
        )
        if response.status_code >= 400:
            raise ShellError("ENDPOINT_UNAVAILABLE", _upstream_message(response))
        body = response.json()
        if not isinstance(body, dict):
            raise ShellError("ENDPOINT_UNAVAILABLE", "Lifecycle did not return an execd endpoint")
        endpoint = body.get("endpoint")
        if not isinstance(endpoint, str) or not endpoint.strip():
            raise ShellError("ENDPOINT_UNAVAILABLE", "Lifecycle did not return an execd endpoint")
        raw_headers = body.get("headers") or {}
        if not isinstance(raw_headers, dict):
            raw_headers = {}
        if shell_endpoint_mode(settings) == "server":
            endpoint = server_proxy_endpoint(settings, sandbox_id)
        headers = execd_request_headers(endpoint, raw_headers, api_key)
        http_base = execd_http_base(endpoint, execd_protocol(settings))
        session_id = await create_pty_session(http_base, headers)
        async with connect_execd(execd_ws_url(http_base, session_id), headers) as execd_ws:
            await bridge_pty(websocket, execd_ws)
    except ShellError as exc:
        await _send_error(websocket, exc.code, exc.message)
    except Exception:
        logger.exception("shell bridge failed for sandbox %s", sandbox_id)
        await _send_error(websocket, "SHELL_FAILED", "Failed to open sandbox shell")
    finally:
        if session_id and http_base:
            await delete_pty_session(http_base, session_id, headers)
        try:
            await websocket.close()
        except Exception:
            return
