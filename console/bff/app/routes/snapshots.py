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

from fastapi import APIRouter, Depends, Request

from app.config import get_settings
from app.deps import get_session_payload, require_tenant_session
from app.lifecycle import LifecycleClient
from app.routes.proxy_utils import passthrough_json
from app.routes.session_keys import tenant_api_key

router = APIRouter(prefix="/snapshots", tags=["snapshots"])


@router.get("")
async def list_snapshots(
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_tenant_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(
        tenant_api_key(payload),
        "GET",
        "/snapshots",
        params=dict(request.query_params),
    )
    return await passthrough_json(resp)


@router.get("/{snapshot_id}")
async def get_snapshot(
    snapshot_id: str,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_tenant_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(tenant_api_key(payload), "GET", f"/snapshots/{snapshot_id}")
    return await passthrough_json(resp)


@router.delete("/{snapshot_id}")
async def delete_snapshot(
    snapshot_id: str,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_tenant_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(tenant_api_key(payload), "DELETE", f"/snapshots/{snapshot_id}")
    return await passthrough_json(resp)
