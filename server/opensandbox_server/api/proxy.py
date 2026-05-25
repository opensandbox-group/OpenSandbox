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
HTTP and WebSocket proxy routes for reaching services inside sandboxes via the lifecycle API.
"""

import logging
from collections.abc import AsyncIterator, Mapping
from typing import Optional

import anyio
import httpx
import websockets
from fastapi import APIRouter, Request, WebSocket, status
from fastapi.exceptions import HTTPException
from fastapi.responses import StreamingResponse
from starlette.websockets import WebSocketDisconnect
from websockets.asyncio.client import ClientConnection
from websockets.typing import Origin

from opensandbox_server.api import lifecycle
from opensandbox_server.api.schema import Endpoint
from opensandbox_server.middleware.auth import SANDBOX_API_KEY_HEADER
from opensandbox_server.services.constants import OPEN_SANDBOX_SECURE_ACCESS_HEADER

logger = logging.getLogger(__name__)

# RFC 2616 Section 13.5.1
HOP_BY_HOP_HEADERS = {
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
}

# Headers that shouldn't be forwarded to untrusted/internal backends
SENSITIVE_HEADERS = {
    "authorization",
    "cookie",
    SANDBOX_API_KEY_HEADER.lower(),
}

# Handled by websockets on the outbound handshake; do not duplicate on additional_headers
WEBSOCKET_HANDSHAKE_HEADERS = {
    "origin",
    "sec-websocket-extensions",
    "sec-websocket-key",
    "sec-websocket-protocol",
    "sec-websocket-version",
}

router = APIRouter(tags=["Sandboxes"])


def _build_proxy_target_url(
    endpoint: Endpoint,
    full_path: str,
    query_string: str,
    *,
    websocket: bool = False,
) -> str:
    """Build the backend URL from an endpoint plus optional path/query suffix.

    For HTTP, ``query_string`` is omitted from the URL so httpx can pass it via ``params=``
    (avoids duplicate encoding issues). For WebSocket, the query is appended to the URI.
    """
    scheme = "ws" if websocket else "http"
    base = endpoint.endpoint.rstrip("/")
    normalized_path = full_path.lstrip("/")
    url = f"{scheme}://{base}"
    if normalized_path:
        url = f"{url}/{normalized_path}"
    if query_string and websocket:
        url = f"{url}?{query_string}"
    return url


def _filter_proxy_headers(
    headers: Mapping[str, str],
    endpoint_headers: Optional[dict[str, str]] = None,
    *,
    extra_excluded: Optional[set[str]] = None,
    connection_header: Optional[str] = None,
) -> dict[str, str]:
    """Drop transport/auth headers while preserving app-level headers.

    Endpoint-resolved headers are merged for routing, except secure-access
    credentials which callers must explicitly provide on server-proxy requests.
    """
    excluded = set(HOP_BY_HOP_HEADERS) | set(SENSITIVE_HEADERS)
    if extra_excluded:
        excluded.update(extra_excluded)
    if connection_header:
        excluded.update(
            h.strip().lower() for h in connection_header.split(",") if h.strip()
        )

    forwarded: dict[str, str] = {}
    for key, value in headers.items():
        key_lower = key.lower()
        if key_lower != "host" and key_lower not in excluded:
            forwarded[key] = value

    if endpoint_headers:
        forwarded.update(
            {
                key: value
                for key, value in endpoint_headers.items()
                if key.lower() != OPEN_SANDBOX_SECURE_ACCESS_HEADER.lower()
            }
        )
    return forwarded


def _schedule_proxy_renew(request: Request | WebSocket, sandbox_id: str) -> None:
    proxy_renew = getattr(request.app.state, "proxy_renew_coordinator", None)
    if proxy_renew is not None:
        proxy_renew.schedule(sandbox_id)


async def _stream_backend_response(resp: httpx.Response) -> AsyncIterator[bytes]:
    """
    Yield backend body chunks and always close the httpx streaming response.

    httpx requires ``await resp.aclose()`` for ``stream=True`` responses so connections
    return to the pool; Starlette's StreamingResponse does not do this automatically.
    """
    try:
        async for chunk in resp.aiter_bytes():
            yield chunk
    finally:
        await resp.aclose()


async def _proxy_http_request(
    request: Request,
    sandbox_id: str,
    port: int,
    full_path: str,
) -> StreamingResponse:
    _schedule_proxy_renew(request, sandbox_id)
    endpoint = lifecycle.sandbox_service.get_endpoint(sandbox_id, port, resolve_internal=True)
    query_string = request.url.query
    target_url = _build_proxy_target_url(endpoint, full_path, query_string, websocket=False)
    client: httpx.AsyncClient = request.app.state.http_client

    try:
        upgrade_header = request.headers.get("Upgrade", "")
        if upgrade_header.lower() == "websocket":
            raise HTTPException(
                status_code=400,
                detail="Websocket upgrade is not supported yet",
            )

        headers = _filter_proxy_headers(
            request.headers,
            endpoint.headers,
            connection_header=request.headers.get("connection"),
        )
        # Inject standard reverse-proxy headers. Check for existing values
        # case-insensitively so an already-present header with any casing
        # (e.g. lowercase "x-forwarded-proto" from an upstream edge) is
        # preserved and we don't emit a duplicate with different casing,
        # which would break chain-safe semantics for downstream backends.
        existing_lower = {key.lower() for key in headers}
        if "x-forwarded-proto" not in existing_lower:
            headers["X-Forwarded-Proto"] = request.url.scheme
        inbound_host = request.headers.get("host", "")
        if inbound_host and "x-forwarded-host" not in existing_lower:
            headers["X-Forwarded-Host"] = inbound_host
        if request.client and "x-forwarded-for" not in existing_lower:
            headers["X-Forwarded-For"] = request.client.host

        stream_body = request.method in ("POST", "PUT", "PATCH", "DELETE")
        req = client.build_request(
            method=request.method,
            url=target_url,
            params=query_string if query_string else None,
            headers=headers,
            content=request.stream() if stream_body else None,
        )

        resp = await client.send(req, stream=True)

        hop_by_hop = set(HOP_BY_HOP_HEADERS)
        connection_header = resp.headers.get("connection")
        if connection_header:
            hop_by_hop.update(
                header.strip().lower()
                for header in connection_header.split(",")
                if header.strip()
            )
        response_headers = {
            key: value
            for key, value in resp.headers.items()
            if key.lower() not in hop_by_hop
        }

        return StreamingResponse(
            content=_stream_backend_response(resp),
            status_code=resp.status_code,
            headers=response_headers,
        )
    except httpx.ConnectError as e:
        raise HTTPException(
            status_code=502,
            detail=f"Could not connect to the backend sandbox {endpoint}: {e}",
        ) from e
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(
            status_code=500, detail=f"An internal error occurred in the proxy: {e}"
        ) from e


async def _fail_client_websocket(websocket: WebSocket, code: int, reason: str = "") -> None:
    """
    Accept then close so the client receives a WebSocket close frame (not only HTTP failure).

    Per ASGI/Starlette, closing before accept yields handshake-level errors instead of
    a proper close code on the WebSocket connection.
    """
    try:
        await websocket.accept()
    except RuntimeError:
        pass
    try:
        await websocket.close(code=code, reason=reason[:123])
    except RuntimeError:
        pass


async def _relay_client_messages(
    websocket: WebSocket,
    backend: ClientConnection,
    cancel_scope: anyio.CancelScope,
) -> None:
    try:
        while True:
            message = await websocket.receive()
            if message["type"] == "websocket.receive":
                if message.get("text") is not None:
                    await backend.send(message["text"])
                elif message.get("bytes") is not None:
                    await backend.send(message["bytes"])
            elif message["type"] == "websocket.disconnect":
                await backend.close(
                    code=message.get("code", status.WS_1000_NORMAL_CLOSURE),
                    reason=message.get("reason") or "",
                )
                return
    except WebSocketDisconnect as exc:
        await backend.close(code=exc.code, reason=getattr(exc, "reason", "") or "")
    finally:
        cancel_scope.cancel()


async def _relay_backend_messages(
    websocket: WebSocket,
    backend: ClientConnection,
    cancel_scope: anyio.CancelScope,
) -> None:
    try:
        while True:
            payload = await backend.recv()
            if isinstance(payload, bytes):
                await websocket.send_bytes(payload)
            else:
                await websocket.send_text(payload)
    except websockets.ConnectionClosed as exc:
        try:
            await websocket.close(
                code=exc.code or status.WS_1000_NORMAL_CLOSURE,
                reason=exc.reason or "",
            )
        except RuntimeError:
            pass
    finally:
        cancel_scope.cancel()


async def _proxy_websocket_request(
    websocket: WebSocket,
    sandbox_id: str,
    port: int,
    full_path: str,
) -> None:
    _schedule_proxy_renew(websocket, sandbox_id)

    try:
        endpoint = lifecycle.sandbox_service.get_endpoint(sandbox_id, port, resolve_internal=True)
    except HTTPException as exc:
        logger.warning(
            "Rejecting websocket proxy request for sandbox=%s port=%s: %s",
            sandbox_id,
            port,
            exc.detail,
        )
        await _fail_client_websocket(
            websocket,
            status.WS_1011_INTERNAL_ERROR,
            str(exc.detail) if exc.detail else "",
        )
        return

    query_string = websocket.url.query or ""
    target_url = _build_proxy_target_url(
        endpoint,
        full_path,
        query_string,
        websocket=True,
    )
    headers = _filter_proxy_headers(
        dict(websocket.headers),
        endpoint.headers,
        extra_excluded=WEBSOCKET_HANDSHAKE_HEADERS,
        connection_header=websocket.headers.get("connection"),
    )
    subprotocols = list(websocket.scope.get("subprotocols", []))
    raw_origin = websocket.headers.get("origin")
    origin: Origin | None = Origin(raw_origin) if raw_origin else None

    try:
        # Do not inherit websockets' default max_size (1 MiB); proxy should not cap payloads.
        async with websockets.connect(
            target_url,
            additional_headers=headers or None,
            subprotocols=subprotocols or None,
            origin=origin,
            max_size=None,
        ) as backend:
            await websocket.accept(subprotocol=backend.subprotocol)
            async with anyio.create_task_group() as task_group:
                task_group.start_soon(
                    _relay_client_messages,
                    websocket,
                    backend,
                    task_group.cancel_scope,
                )
                task_group.start_soon(
                    _relay_backend_messages,
                    websocket,
                    backend,
                    task_group.cancel_scope,
                )
    except websockets.InvalidStatus as exc:
        logger.warning(
            "Backend websocket handshake failed for sandbox=%s port=%s: %s",
            sandbox_id,
            port,
            exc,
        )
        await _fail_client_websocket(websocket, status.WS_1008_POLICY_VIOLATION, "")
    except OSError as exc:
        logger.warning(
            "Could not connect websocket proxy for sandbox=%s port=%s: %s",
            sandbox_id,
            port,
            exc,
        )
        await _fail_client_websocket(websocket, status.WS_1011_INTERNAL_ERROR, "")
    except Exception:
        logger.exception(
            "Unexpected websocket proxy failure for sandbox=%s port=%s",
            sandbox_id,
            port,
        )
        await _fail_client_websocket(websocket, status.WS_1011_INTERNAL_ERROR, "")


@router.api_route(
    "/sandboxes/{sandbox_id}/proxy/{port}",
    methods=["GET", "POST", "PUT", "DELETE", "PATCH"],
)
async def proxy_sandbox_endpoint_root(
    request: Request,
    sandbox_id: str,
    port: int,
):
    """Proxy HTTP requests targeting the backend root path."""
    return await _proxy_http_request(request, sandbox_id, port, "")


@router.api_route(
    "/sandboxes/{sandbox_id}/proxy/{port}/{full_path:path}",
    methods=["GET", "POST", "PUT", "DELETE", "PATCH"],
)
async def proxy_sandbox_endpoint_request(
    request: Request,
    sandbox_id: str,
    port: int,
    full_path: str,
):
    """Proxy HTTP requests to sandbox-backed services."""
    return await _proxy_http_request(request, sandbox_id, port, full_path)


@router.websocket("/sandboxes/{sandbox_id}/proxy/{port}")
async def proxy_sandbox_endpoint_root_websocket(
    websocket: WebSocket,
    sandbox_id: str,
    port: int,
):
    """Proxy WebSocket connections targeting the backend root path."""
    await _proxy_websocket_request(websocket, sandbox_id, port, "")


@router.websocket("/sandboxes/{sandbox_id}/proxy/{port}/{full_path:path}")
async def proxy_sandbox_endpoint_request_websocket(
    websocket: WebSocket,
    sandbox_id: str,
    port: int,
    full_path: str,
):
    """Proxy WebSocket connections to sandbox-backed services."""
    await _proxy_websocket_request(websocket, sandbox_id, port, full_path)
