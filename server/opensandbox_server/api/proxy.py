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

import hmac
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
from opensandbox_server.services.constants import OPEN_SANDBOX_EGRESS_AUTH_HEADER, OPEN_SANDBOX_SECURE_ACCESS_HEADER
from opensandbox_server.tenants.context import set_current_tenant
from opensandbox_server.tenants.provider import TenantProviderUnavailable

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
    OPEN_SANDBOX_SECURE_ACCESS_HEADER.lower(),
}

FORWARDED_HEADERS = {
    "forwarded",
    "x-forwarded-for",
    "x-forwarded-host",
    "x-forwarded-port",
    "x-forwarded-proto",
    "x-real-ip",
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
    excluded = set(HOP_BY_HOP_HEADERS) | set(SENSITIVE_HEADERS) | set(FORWARDED_HEADERS)
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
        endpoint_header_excluded = {
            OPEN_SANDBOX_SECURE_ACCESS_HEADER.lower(),
            OPEN_SANDBOX_EGRESS_AUTH_HEADER.lower(),
        } | FORWARDED_HEADERS
        forwarded.update(
            {
                key: value
                for key, value in endpoint_headers.items()
                if key.lower() not in endpoint_header_excluded
            }
        )
    return forwarded


def _set_forwarded_headers(
    headers: dict[str, str], request: Request | WebSocket
) -> None:
    """Rebuild proxy headers from the connection observed by this server."""
    scheme = request.url.scheme.lower()
    if scheme == "ws":
        scheme = "http"
    elif scheme == "wss":
        scheme = "https"
    headers["X-Forwarded-Proto"] = scheme

    inbound_host = request.headers.get("host", "")
    if inbound_host:
        headers["X-Forwarded-Host"] = inbound_host
    if request.client:
        headers["X-Forwarded-For"] = request.client.host


def _schedule_proxy_renew(request: Request | WebSocket, sandbox_id: str) -> None:
    proxy_renew = getattr(request.app.state, "proxy_renew_coordinator", None)
    if proxy_renew is not None:
        proxy_renew.schedule(sandbox_id)


async def _authenticate_websocket_tenant(websocket: WebSocket) -> bool:
    """Authenticate WebSocket connections in multi-tenant mode.

    BaseHTTPMiddleware only intercepts HTTP requests, so WebSocket
    connections must be authenticated here to establish tenant context.
    Returns True if the request is authorized (or single-tenant mode).
    """
    import asyncio

    provider = getattr(websocket.app.state, "tenant_provider", None)
    if provider is None:
        return True

    api_key = websocket.headers.get(SANDBOX_API_KEY_HEADER)
    if not api_key:
        await _fail_client_websocket(
            websocket, status.WS_1008_POLICY_VIOLATION, "missing API key"
        )
        return False

    try:
        tenant = await asyncio.to_thread(provider.lookup, api_key)
    except TenantProviderUnavailable:
        await _fail_client_websocket(
            websocket, status.WS_1011_INTERNAL_ERROR, "tenant provider unavailable"
        )
        return False

    if tenant is None:
        await _fail_client_websocket(
            websocket, status.WS_1008_POLICY_VIOLATION, "invalid API key"
        )
        return False

    set_current_tenant(tenant)
    return True


async def _stream_backend_response(resp: httpx.Response) -> AsyncIterator[bytes]:
    """
    Yield backend body chunks without httpx content decoding and always close the response.

    httpx requires ``await resp.aclose()`` for ``stream=True`` responses so connections
    return to the pool; Starlette's StreamingResponse does not do this automatically.
    Use ``aiter_raw`` so forwarded ``content-encoding`` headers still match the body bytes.
    """
    try:
        async for chunk in resp.aiter_raw():
            yield chunk
    finally:
        await resp.aclose()


def _verify_secure_access(endpoint: Endpoint, caller_headers: Mapping[str, str]) -> None:
    """Enforce OpenSandbox-Secure-Access validation on server-proxy requests.

    When endpoint resolution returns a secure-access token, the caller must
    supply the same header value.  Raises 401 for missing or mismatched tokens.
    Uses constant-time comparison to avoid timing side-channels.
    """
    if not endpoint.headers:
        return
    expected_token = endpoint.headers.get(OPEN_SANDBOX_SECURE_ACCESS_HEADER)
    if not expected_token:
        return
    caller_token = None
    for key, value in caller_headers.items():
        if key.lower() == OPEN_SANDBOX_SECURE_ACCESS_HEADER.lower():
            caller_token = value
            break
    if not caller_token or not hmac.compare_digest(
        caller_token.encode(), expected_token.encode()
    ):
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail={
                "code": "MISSING_OR_INVALID_SECURE_ACCESS",
                "message": (
                    "This sandbox requires the "
                    f"{OPEN_SANDBOX_SECURE_ACCESS_HEADER} header for access."
                ),
            },
        )


async def _proxy_http_request(
    request: Request,
    sandbox_id: str,
    port: int,
    full_path: str,
) -> StreamingResponse:
    endpoint = lifecycle.sandbox_service.get_endpoint(sandbox_id, port, resolve_internal=True)
    _verify_secure_access(endpoint, request.headers)
    _schedule_proxy_renew(request, sandbox_id)
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
        # Forwarded headers are stripped above and rebuilt from the connection
        # observed by this trusted proxy, so clients cannot spoof transport state.
        _set_forwarded_headers(headers, request)

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
    if not await _authenticate_websocket_tenant(websocket):
        return

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

    try:
        _verify_secure_access(endpoint, dict(websocket.headers))
    except HTTPException:
        await _fail_client_websocket(
            websocket,
            status.WS_1008_POLICY_VIOLATION,
            "Missing or invalid secure-access token",
        )
        return

    _schedule_proxy_renew(websocket, sandbox_id)
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
    _set_forwarded_headers(headers, websocket)
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
