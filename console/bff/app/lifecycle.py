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

import httpx

from app.config import Settings


class LifecycleClient:
    def __init__(self, settings: Settings) -> None:
        self._base = settings.lifecycle_api_base.rstrip("/")
        self._timeout = settings.bff_http_timeout_seconds

    def _headers(self, api_key: str) -> dict[str, str]:
        return {"OPEN-SANDBOX-API-KEY": api_key}

    async def request(
        self,
        api_key: str,
        method: str,
        path: str,
        *,
        params: dict[str, Any] | None = None,
        json_body: dict[str, Any] | None = None,
    ) -> httpx.Response:
        url = f"{self._base}{path}"
        async with httpx.AsyncClient(timeout=self._timeout) as client:
            return await client.request(
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
        async with httpx.AsyncClient(timeout=self._timeout) as client:
            resp = await client.get(f"{self._server_root()}/health")
            resp.raise_for_status()
            return resp.json()

    async def get_version(self) -> dict[str, Any]:
        async with httpx.AsyncClient(timeout=self._timeout) as client:
            resp = await client.get(f"{self._server_root()}/version")
            resp.raise_for_status()
            return resp.json()
