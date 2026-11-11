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

from typing import Any
from urllib.parse import quote

import httpx
from fastapi import HTTPException, status

from app.config import Settings

_shared_client: httpx.AsyncClient | None = None


def bind_http_client(client: httpx.AsyncClient | None) -> None:
    """Install the process-wide client created in the app lifespan."""
    global _shared_client
    _shared_client = client


def normalize_upstream_path(path: str) -> str:
    """Quote each path segment so ids cannot rewrite the query, fragment, or parent path."""
    if not path.startswith("/"):
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={"code": "INVALID_PATH", "message": "Upstream path must be absolute"},
        )
    quoted: list[str] = []
    for segment in path.split("/"):
        if segment == "":
            quoted.append("")
            continue
        if segment in {".", ".."}:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={"code": "INVALID_PATH", "message": "Upstream path segment is not allowed"},
            )
        quoted.append(quote(segment, safe=""))
    return "/".join(quoted)


class LifecycleClient:
    def __init__(self, settings: Settings) -> None:
        self._base = settings.lifecycle_api_base.rstrip("/")
        self._timeout = settings.bff_http_timeout_seconds

    def _headers(self, api_key: str) -> dict[str, str]:
        return {"OPEN-SANDBOX-API-KEY": api_key}

    def _open_client(self) -> tuple[httpx.AsyncClient, bool]:
        if _shared_client is not None:
            return _shared_client, False
        return httpx.AsyncClient(timeout=self._timeout), True

    async def _send(self, method: str, url: str, **kwargs: Any) -> httpx.Response:
        client, owns = self._open_client()
        try:
            return await client.request(method, url, **kwargs)
        except httpx.TimeoutException as exc:
            raise HTTPException(
                status_code=status.HTTP_504_GATEWAY_TIMEOUT,
                detail={"code": "UPSTREAM_TIMEOUT", "message": "Lifecycle request timed out"},
            ) from exc
        except httpx.HTTPError as exc:
            raise HTTPException(
                status_code=status.HTTP_502_BAD_GATEWAY,
                detail={"code": "UPSTREAM_UNAVAILABLE", "message": "Lifecycle request failed"},
            ) from exc
        finally:
            if owns:
                await client.aclose()

    async def request(
        self,
        api_key: str,
        method: str,
        path: str,
        *,
        params: dict[str, Any] | None = None,
        json_body: dict[str, Any] | None = None,
    ) -> httpx.Response:
        url = f"{self._base}{normalize_upstream_path(path)}"
        return await self._send(
            method,
            url,
            headers=self._headers(api_key),
            params=params,
            json=json_body,
        )

    def _server_root(self) -> str:
        if self._base.endswith("/v1"):
            return self._base[: -len("/v1")]
        return self._base

    async def get_health(self) -> dict[str, Any]:
        resp = await self._send("GET", f"{self._server_root()}/health")
        resp.raise_for_status()
        return resp.json()

    async def get_version(self) -> dict[str, Any]:
        resp = await self._send("GET", f"{self._server_root()}/version")
        resp.raise_for_status()
        return resp.json()
