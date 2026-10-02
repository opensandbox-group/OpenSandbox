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
from app.runtime import attach_runtime_summary
from app.routes.proxy_utils import passthrough_json
from app.nodeagent_archive import fetch_archive_logs
from app.history import tasks as history_tasks
from app.routes.session_keys import tenant_api_key, tenant_namespace_for_session

router = APIRouter(prefix="/sandboxes", tags=["sandboxes"])


@router.get("")
async def list_sandboxes(
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_tenant_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(
        tenant_api_key(payload),
        "GET",
        "/sandboxes",
        params=dict(request.query_params),
    )
    data = await passthrough_json(resp)
    if isinstance(data, dict) and "items" in data:
        enriched = [attach_runtime_summary(item) for item in data["items"]]
        data["items"] = enriched
        for item in enriched:
            history_tasks.schedule_upsert(payload, item)
    return data


@router.get("/{sandbox_id}")
async def get_sandbox(
    sandbox_id: str,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_tenant_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(tenant_api_key(payload), "GET", f"/sandboxes/{sandbox_id}")
    data = await passthrough_json(resp)
    if isinstance(data, dict):
        enriched = attach_runtime_summary(data)
        history_tasks.schedule_upsert(payload, enriched)
        return enriched
    return data


@router.post("")
async def create_sandbox(
    body: dict[str, Any],
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_tenant_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(tenant_api_key(payload), "POST", "/sandboxes", json_body=body)
    data = await passthrough_json(resp)
    if isinstance(data, dict):
        enriched = attach_runtime_summary(data)
        history_tasks.schedule_upsert(payload, enriched, create_request=body)
        return enriched
    return data


@router.delete("/{sandbox_id}")
async def delete_sandbox(
    sandbox_id: str,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_tenant_session(payload)
    client = LifecycleClient(get_settings())
    api_key = tenant_api_key(payload)
    final_sandbox: dict | None = None
    get_resp = await client.request(api_key, "GET", f"/sandboxes/{sandbox_id}")
    if get_resp.status_code < 400:
        try:
            final_sandbox = get_resp.json()
        except Exception:
            final_sandbox = None
    resp = await client.request(api_key, "DELETE", f"/sandboxes/{sandbox_id}")
    history_tasks.schedule_mark_deleted(
        sandbox_id,
        tenant=str(payload.get("tenant") or ""),
        namespace=str(payload.get("namespace") or ""),
        final_sandbox=final_sandbox if isinstance(final_sandbox, dict) else None,
    )
    return await passthrough_json(resp)


@router.post("/{sandbox_id}/renew-expiration")
async def renew_sandbox(
    sandbox_id: str,
    body: dict[str, Any],
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_tenant_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(
        tenant_api_key(payload),
        "POST",
        f"/sandboxes/{sandbox_id}/renew-expiration",
        json_body=body,
    )
    data = await passthrough_json(resp)
    if isinstance(data, dict):
        enriched = attach_runtime_summary(data)
        history_tasks.schedule_upsert(payload, enriched)
        return enriched
    return data


@router.post("/{sandbox_id}/pause")
async def pause_sandbox(
    sandbox_id: str,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_tenant_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(
        tenant_api_key(payload),
        "POST",
        f"/sandboxes/{sandbox_id}/pause",
    )
    return await passthrough_json(resp)


@router.post("/{sandbox_id}/resume")
async def resume_sandbox(
    sandbox_id: str,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_tenant_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(
        tenant_api_key(payload),
        "POST",
        f"/sandboxes/{sandbox_id}/resume",
    )
    return await passthrough_json(resp)


@router.post("/{sandbox_id}/snapshots")
async def create_snapshot(
    sandbox_id: str,
    body: dict[str, Any] | None = None,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_tenant_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(
        tenant_api_key(payload),
        "POST",
        f"/sandboxes/{sandbox_id}/snapshots",
        json_body=body or {},
    )
    return await passthrough_json(resp)


@router.get("/{sandbox_id}/logs/archive")
async def get_sandbox_archive_logs(
    sandbox_id: str,
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_tenant_session(payload)
    namespace = tenant_namespace_for_session(payload)
    max_bytes = request.query_params.get("maxBytes")
    return await fetch_archive_logs(
        get_settings(),
        namespace,
        sandbox_id,
        max_bytes=int(max_bytes) if max_bytes else None,
    )


@router.get("/{sandbox_id}/diagnostics/logs")
async def get_sandbox_diagnostic_logs(
    sandbox_id: str,
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_tenant_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(
        tenant_api_key(payload),
        "GET",
        f"/sandboxes/{sandbox_id}/diagnostics/logs",
        params=dict(request.query_params),
    )
    return await passthrough_json(resp)


@router.get("/{sandbox_id}/diagnostics/events")
async def get_sandbox_diagnostic_events(
    sandbox_id: str,
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_tenant_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(
        tenant_api_key(payload),
        "GET",
        f"/sandboxes/{sandbox_id}/diagnostics/events",
        params=dict(request.query_params),
    )
    return await passthrough_json(resp)


@router.get("/{sandbox_id}/endpoints/{port}")
async def get_endpoint(
    sandbox_id: str,
    port: int,
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> Any:
    require_tenant_session(payload)
    client = LifecycleClient(get_settings())
    resp = await client.request(
        tenant_api_key(payload),
        "GET",
        f"/sandboxes/{sandbox_id}/endpoints/{port}",
        params=dict(request.query_params),
    )
    return await passthrough_json(resp)
