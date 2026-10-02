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

import asyncio
from datetime import datetime, timezone
from typing import Any

from fastapi import APIRouter, Depends, HTTPException, Request, status

from app.config import get_settings
from app.deps import get_session_payload, require_admin_session
from app.lifecycle import LifecycleClient
from app.routes.proxy_utils import passthrough_json
from app.k8s_platform import probe_platform_deployment
from app.k8s_probe import probe_node_agent
from app.k8s_resources import list_events, list_workloads
from app.nodeagent_archive import fetch_archive_logs
from app.routes.admin_proxy import admin_get_sandbox, admin_lifecycle_request
from app.routes.session_keys import api_key_for_tenant_name, tenant_namespace_for_name
from app.runtime import aggregate_runtime_stats, attach_runtime_summary
from app.history import tasks as history_tasks
from app.tenants import load_tenants

router = APIRouter(prefix="/admin", tags=["admin"])


@router.get("/tenants")
async def list_configured_tenants(
    payload: dict = Depends(get_session_payload),
) -> dict[str, Any]:
    """Tenant names from BFF tenants.toml (for Admin UI tenant picker)."""
    require_admin_session(payload)
    settings = get_settings()
    tenants = load_tenants(settings.tenants_toml_path)
    return {
        "items": [{"name": t.name, "namespace": t.namespace} for t in tenants],
    }


@router.get("/sandboxes")
async def admin_list_sandboxes(
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> dict[str, Any]:
    require_admin_session(payload)
    settings = get_settings()
    tenants = load_tenants(settings.tenants_toml_path)
    filter_tenant = request.query_params.get("tenant")
    if filter_tenant:
        tenants = [t for t in tenants if t.name == filter_tenant]

    params = dict(request.query_params)
    params.pop("tenant", None)

    client = LifecycleClient(settings)
    items: list[dict[str, Any]] = []
    errors: list[dict[str, str]] = []

    async def fetch_one(tenant_name: str, namespace: str, api_key: str) -> None:
        resp = await client.request(api_key, "GET", "/sandboxes", params=params)
        if resp.status_code >= 400:
            try:
                body = resp.json()
            except Exception:
                body = {"code": "UPSTREAM_ERROR", "message": resp.text or resp.reason_phrase}
            errors.append(
                {
                    "tenant": tenant_name,
                    "code": str(body.get("code", "UPSTREAM_ERROR")),
                    "message": str(body.get("message", resp.reason_phrase)),
                },
            )
            return
        data = resp.json()
        if not isinstance(data, dict):
            errors.append({"tenant": tenant_name, "code": "UPSTREAM_ERROR", "message": "Invalid response"})
            return
        for item in data.get("items") or []:
            enriched = attach_runtime_summary(item)
            enriched["tenant"] = tenant_name
            enriched["namespace"] = namespace
            items.append(enriched)
            history_tasks.schedule_upsert(
                {"role": "admin", "tenant": tenant_name, "namespace": namespace},
                enriched,
                source="console-admin",
            )

    await asyncio.gather(*[fetch_one(t.name, t.namespace, t.api_key) for t in tenants])

    pagination = {
        "page": int(params.get("page", 1) or 1),
        "pageSize": int(params.get("pageSize", params.get("page_size", 20)) or 20),
        "totalItems": len(items),
        "totalPages": 1,
        "hasNextPage": False,
        "tenantErrors": errors,
    }
    return {"items": items, "pagination": pagination}


@router.get("/sandboxes/{sandbox_id}")
async def admin_get_sandbox_route(
    sandbox_id: str,
    request: Request,
    payload: dict = Depends(get_session_payload),
):
    require_admin_session(payload)
    tenant = _require_tenant_query(request)
    return await admin_get_sandbox(tenant, sandbox_id)


@router.delete("/sandboxes/{sandbox_id}")
async def admin_delete_sandbox(
    sandbox_id: str,
    request: Request,
    payload: dict = Depends(get_session_payload),
):
    require_admin_session(payload)
    tenant = _require_tenant_query(request)
    ns = tenant_namespace_for_name(tenant)
    final_sandbox = await admin_get_sandbox(tenant, sandbox_id)
    result = await admin_lifecycle_request(tenant, "DELETE", f"/sandboxes/{sandbox_id}", request=request)
    history_tasks.schedule_mark_deleted(
        sandbox_id,
        tenant=tenant,
        namespace=ns,
        final_sandbox=final_sandbox if isinstance(final_sandbox, dict) else None,
    )
    return result


@router.post("/sandboxes/{sandbox_id}/renew-expiration")
async def admin_renew_sandbox(
    sandbox_id: str,
    body: dict[str, Any],
    request: Request,
    payload: dict = Depends(get_session_payload),
):
    require_admin_session(payload)
    tenant = _require_tenant_query(request)
    return await admin_lifecycle_request(
        tenant,
        "POST",
        f"/sandboxes/{sandbox_id}/renew-expiration",
        request=request,
        json_body=body,
    )


@router.post("/sandboxes/{sandbox_id}/pause")
async def admin_pause_sandbox(
    sandbox_id: str,
    request: Request,
    payload: dict = Depends(get_session_payload),
):
    require_admin_session(payload)
    tenant = _require_tenant_query(request)
    return await admin_lifecycle_request(tenant, "POST", f"/sandboxes/{sandbox_id}/pause", request=request)


@router.post("/sandboxes/{sandbox_id}/resume")
async def admin_resume_sandbox(
    sandbox_id: str,
    request: Request,
    payload: dict = Depends(get_session_payload),
):
    require_admin_session(payload)
    tenant = _require_tenant_query(request)
    return await admin_lifecycle_request(tenant, "POST", f"/sandboxes/{sandbox_id}/resume", request=request)


@router.get("/sandboxes/{sandbox_id}/endpoints/{port}")
async def admin_sandbox_endpoint(
    sandbox_id: str,
    port: int,
    request: Request,
    payload: dict = Depends(get_session_payload),
):
    require_admin_session(payload)
    tenant = _require_tenant_query(request)
    return await admin_lifecycle_request(
        tenant,
        "GET",
        f"/sandboxes/{sandbox_id}/endpoints/{port}",
        request=request,
    )


@router.post("/sandboxes/{sandbox_id}/snapshots")
async def admin_create_snapshot(
    sandbox_id: str,
    body: dict[str, Any],
    request: Request,
    payload: dict = Depends(get_session_payload),
):
    require_admin_session(payload)
    tenant = _require_tenant_query(request)
    return await admin_lifecycle_request(
        tenant,
        "POST",
        f"/sandboxes/{sandbox_id}/snapshots",
        request=request,
        json_body=body,
    )


@router.get("/snapshots")
async def admin_list_snapshots(
    request: Request,
    payload: dict = Depends(get_session_payload),
):
    require_admin_session(payload)
    tenant = _require_tenant_query(request)
    return await admin_lifecycle_request(tenant, "GET", "/snapshots", request=request)


@router.get("/snapshots/{snapshot_id}")
async def admin_get_snapshot(
    snapshot_id: str,
    request: Request,
    payload: dict = Depends(get_session_payload),
):
    require_admin_session(payload)
    tenant = _require_tenant_query(request)
    return await admin_lifecycle_request(tenant, "GET", f"/snapshots/{snapshot_id}", request=request)


@router.delete("/snapshots/{snapshot_id}")
async def admin_delete_snapshot(
    snapshot_id: str,
    request: Request,
    payload: dict = Depends(get_session_payload),
):
    require_admin_session(payload)
    tenant = _require_tenant_query(request)
    return await admin_lifecycle_request(tenant, "DELETE", f"/snapshots/{snapshot_id}", request=request)


@router.get("/k8s/workloads")
async def admin_k8s_workloads(
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> dict[str, Any]:
    require_admin_session(payload)
    settings = get_settings()
    qp = request.query_params
    limit = int(qp.get("limit", 200) or 200)
    return await list_workloads(
        settings,
        tenant=qp.get("tenant"),
        namespace=qp.get("namespace"),
        sandbox_id=qp.get("sandboxId"),
        limit=limit,
    )


@router.get("/k8s/events")
async def admin_k8s_events(
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> dict[str, Any]:
    require_admin_session(payload)
    settings = get_settings()
    qp = request.query_params
    limit = int(qp.get("limit", 100) or 100)
    return await list_events(
        settings,
        tenant=qp.get("tenant"),
        namespace=qp.get("namespace"),
        sandbox_id=qp.get("sandboxId"),
        limit=limit,
    )


@router.get("/stats/runtime")
async def admin_runtime_stats(
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> dict[str, Any]:
    require_admin_session(payload)
    listed = await admin_list_sandboxes(request, payload)
    return aggregate_runtime_stats(listed.get("items") or [])


def _require_tenant_query(request: Request) -> str:
    tenant = request.query_params.get("tenant")
    if not tenant or not tenant.strip():
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": "TENANT_REQUIRED",
                "message": "Query parameter tenant is required for admin diagnostics",
            },
        )
    return tenant.strip()


@router.get("/sandboxes/{sandbox_id}/diagnostics/logs")
async def admin_sandbox_diagnostic_logs(
    sandbox_id: str,
    request: Request,
    payload: dict = Depends(get_session_payload),
):
    require_admin_session(payload)
    tenant = _require_tenant_query(request)
    api_key = api_key_for_tenant_name(tenant)
    params = dict(request.query_params)
    params.pop("tenant", None)
    client = LifecycleClient(get_settings())
    resp = await client.request(
        api_key,
        "GET",
        f"/sandboxes/{sandbox_id}/diagnostics/logs",
        params=params,
    )
    return await passthrough_json(resp)


@router.get("/sandboxes/{sandbox_id}/diagnostics/events")
async def admin_sandbox_diagnostic_events(
    sandbox_id: str,
    request: Request,
    payload: dict = Depends(get_session_payload),
):
    require_admin_session(payload)
    tenant = _require_tenant_query(request)
    api_key = api_key_for_tenant_name(tenant)
    params = dict(request.query_params)
    params.pop("tenant", None)
    client = LifecycleClient(get_settings())
    resp = await client.request(
        api_key,
        "GET",
        f"/sandboxes/{sandbox_id}/diagnostics/events",
        params=params,
    )
    return await passthrough_json(resp)


@router.get("/platform/summary")
async def admin_platform_summary(
    payload: dict = Depends(get_session_payload),
) -> dict[str, Any]:
    require_admin_session(payload)
    client = LifecycleClient(get_settings())
    health: dict[str, Any] | None = None
    version: dict[str, Any] | None = None
    health_error: str | None = None
    version_error: str | None = None
    try:
        health = await client.get_health()
    except Exception as exc:
        health_error = str(exc)
    try:
        version = await client.get_version()
    except Exception as exc:
        version_error = str(exc)

    return {
        "asOf": datetime.now(timezone.utc).isoformat(),
        "server": {
            "health": health,
            "healthError": health_error,
            "version": version,
            "versionError": version_error,
        },
        "components": {
            "ingress": await probe_platform_deployment(
                get_settings(),
                component="ingress",
                deployment_name=get_settings().bff_k8s_ingress_deployment,
            ),
            "controller": await probe_platform_deployment(
                get_settings(),
                component="controller",
                deployment_name=get_settings().bff_k8s_controller_deployment,
            ),
            "nodeAgent": await probe_node_agent(get_settings()),
            "console": {"status": "ok", "note": "BFF session active"},
        },
    }


@router.get("/sandboxes/{sandbox_id}/logs/archive")
async def admin_sandbox_archive_logs(
    sandbox_id: str,
    request: Request,
    payload: dict = Depends(get_session_payload),
):
    require_admin_session(payload)
    tenant = _require_tenant_query(request)
    namespace = tenant_namespace_for_name(tenant)
    max_bytes = request.query_params.get("maxBytes")
    return await fetch_archive_logs(
        get_settings(),
        namespace,
        sandbox_id,
        max_bytes=int(max_bytes) if max_bytes else None,
    )
