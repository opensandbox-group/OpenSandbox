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


async def _pool_proxy(method: str, path: str, **kwargs: Any) -> Any:
    """Pool API is not tenant-scoped. Try each configured key until one is accepted.

    A revoked or rotated first key must not fail every pool call while another
    key in tenants.toml is still valid. 401/403 tries the next key; other
    statuses are the upstream answer.
    """
    settings = get_settings()
    tenants = load_tenants(settings.tenants_toml_path)
    if not tenants:
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail={"code": "NO_TENANTS", "message": "No tenants configured for pool proxy"},
        )
    client = LifecycleClient(settings)
    last = None
    for tenant in tenants:
        for api_key in tenant.api_keys or (tenant.api_key,):
            resp = await client.request(api_key, method, path, **kwargs)
            if resp.status_code not in (401, 403):
                return await passthrough_json(resp)
            last = resp
    if last is None:
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail={"code": "NO_TENANTS", "message": "No tenants configured for pool proxy"},
        )
    return await passthrough_json(last)


@router.get("")
async def list_pools(
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_admin_session(payload)
    return await _pool_proxy("GET", "/pools", params=dict(request.query_params))


@router.post("")
async def create_pool(
    body: dict[str, Any],
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_admin_session(payload)
    return await _pool_proxy("POST", "/pools", json_body=body)


@router.get("/{pool_name}")
async def get_pool(
    pool_name: str,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_admin_session(payload)
    return await _pool_proxy("GET", f"/pools/{pool_name}")


@router.put("/{pool_name}")
async def update_pool(
    pool_name: str,
    body: dict[str, Any],
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_admin_session(payload)
    return await _pool_proxy("PUT", f"/pools/{pool_name}", json_body=body)


@router.delete("/{pool_name}")
async def delete_pool(
    pool_name: str,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_admin_session(payload)
    return await _pool_proxy("DELETE", f"/pools/{pool_name}")
