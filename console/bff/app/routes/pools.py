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

from fastapi import APIRouter, Depends, HTTPException, Request, status

from app.config import get_settings
from app.deps import get_session_payload, require_admin_session
from app.lifecycle import LifecycleClient
from app.routes.proxy_utils import passthrough_json
from app.tenants import load_tenants

router = APIRouter(prefix="/pools", tags=["pools"])


def _pool_proxy_api_key() -> str:
    """Pool API is not tenant-scoped; any configured tenant key satisfies Lifecycle auth."""
    settings = get_settings()
    tenants = load_tenants(settings.tenants_toml_path)
    if not tenants:
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail={"code": "NO_TENANTS", "message": "No tenants configured for pool proxy"},
        )
    return tenants[0].api_key


@router.get("")
async def list_pools(
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_admin_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(
        _pool_proxy_api_key(),
        "GET",
        "/pools",
        params=dict(request.query_params),
    )
    return await passthrough_json(resp)


@router.post("")
async def create_pool(
    body: dict[str, Any],
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_admin_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(_pool_proxy_api_key(), "POST", "/pools", json_body=body)
    return await passthrough_json(resp)


@router.get("/{pool_name}")
async def get_pool(
    pool_name: str,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_admin_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(_pool_proxy_api_key(), "GET", f"/pools/{pool_name}")
    return await passthrough_json(resp)


@router.put("/{pool_name}")
async def update_pool(
    pool_name: str,
    body: dict[str, Any],
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_admin_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(
        _pool_proxy_api_key(),
        "PUT",
        f"/pools/{pool_name}",
        json_body=body,
    )
    return await passthrough_json(resp)


@router.delete("/{pool_name}")
async def delete_pool(
    pool_name: str,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_admin_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(_pool_proxy_api_key(), "DELETE", f"/pools/{pool_name}")
    return await passthrough_json(resp)
