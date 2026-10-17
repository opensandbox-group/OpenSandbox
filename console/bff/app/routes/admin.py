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
from app.routes.admin_proxy import admin_get_sandbox, admin_lifecycle_request
from app.routes.session_keys import api_key_for_tenant_name
from app.runtime import aggregate_runtime_stats, attach_runtime_summary
from app.tenants import load_tenants

router = APIRouter(prefix="/admin", tags=["admin"])

# Lifecycle rejects pageSize above 200. The admin list forwards one page per tenant;
# runtime stats walk every page up to this cap.
_UPSTREAM_PAGE_SIZE = 200
_MAX_UPSTREAM_PAGES = 50


def _query_int(raw: str | None, default: int, name: str, *, maximum: int | None = None) -> int:
    if raw is None or raw == "":
        value = default
    else:
        try:
            value = int(raw)
        except ValueError as exc:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={"code": "INVALID_QUERY", "message": f"{name} must be an integer"},
            ) from exc
    if value < 1 or (maximum is not None and value > maximum):
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={"code": "INVALID_QUERY", "message": f"{name} is out of range"},
        )
    return value


def _upstream_failure(tenant_name: str, exc: HTTPException) -> dict[str, str]:
    detail = exc.detail if isinstance(exc.detail, dict) else {}
    return {
        "tenant": tenant_name,
        "code": str(detail.get("code") or "UPSTREAM_ERROR"),
        "message": str(detail.get("message") or exc.detail),
    }


def _response_failure(tenant_name: str, resp: Any) -> dict[str, str]:
    try:
        body = resp.json()
    except Exception:
        body = {"code": "UPSTREAM_ERROR", "message": resp.text or resp.reason_phrase}
    if not isinstance(body, dict):
        body = {}
    return {
        "tenant": tenant_name,
        "code": str(body.get("code", "UPSTREAM_ERROR")),
        "message": str(body.get("message", resp.reason_phrase)),
    }


def _page_continues(data: dict[str, Any], batch_len: int, page_size: int) -> bool:
    pagination = data.get("pagination")
    if isinstance(pagination, dict) and "hasNextPage" in pagination:
        return bool(pagination["hasNextPage"])
    return batch_len >= page_size


def _selected_tenants(request: Request) -> list[Any]:
    settings = get_settings()
    tenants = load_tenants(settings.tenants_toml_path)
    filter_tenant = request.query_params.get("tenant")
    if filter_tenant:
        api_key_for_tenant_name(filter_tenant)
        tenants = [tenant for tenant in tenants if tenant.name == filter_tenant]
    return tenants


def _enrich_items(
    data: dict[str, Any],
    tenant_name: str,
    namespace: str,
    now: datetime,
) -> list[dict[str, Any]]:
    enriched: list[dict[str, Any]] = []
    for item in data.get("items") or []:
        if not isinstance(item, dict):
            continue
        row = attach_runtime_summary(item, now)
        row["tenant"] = tenant_name
        row["namespace"] = namespace
        enriched.append(row)
    return enriched


@router.get("/tenants")
async def admin_list_tenants(
    payload: dict = Depends(get_session_payload),
) -> dict[str, Any]:
    """Names and namespaces only. Tenant API keys stay on the BFF."""
    require_admin_session(payload)
    tenants = load_tenants(get_settings().tenants_toml_path)
    return {
        "items": [{"name": tenant.name, "namespace": tenant.namespace} for tenant in tenants],
    }


@router.get("/sandboxes")
async def admin_list_sandboxes(
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> dict[str, Any]:
    """One upstream page per tenant.

    ``totalItems`` is the number of rows in this response, not the cross-tenant
    total. ``capped`` is true when any tenant still has another page.
    """
    require_admin_session(payload)
    tenants = _selected_tenants(request)
    params = {key: value for key, value in request.query_params.items() if key != "tenant"}
    page = _query_int(params.get("page"), 1, "page")
    page_size = _query_int(
        params.get("pageSize") or params.get("page_size"),
        20,
        "pageSize",
        maximum=_UPSTREAM_PAGE_SIZE,
    )
    params.pop("page_size", None)
    params["page"] = str(page)
    params["pageSize"] = str(page_size)

    client = LifecycleClient(get_settings())
    now = datetime.now(timezone.utc)

    async def fetch_one(tenant: Any) -> tuple[list[dict[str, Any]], dict[str, str] | None, bool]:
        try:
            resp = await client.request(tenant.api_key, "GET", "/sandboxes", params=params)
        except HTTPException as exc:
            return [], _upstream_failure(tenant.name, exc), False
        if resp.status_code >= 400:
            return [], _response_failure(tenant.name, resp), False
        data = resp.json()
        if not isinstance(data, dict):
            return [], {"tenant": tenant.name, "code": "UPSTREAM_ERROR", "message": "Invalid response"}, False
        batch = data.get("items") or []
        return (
            _enrich_items(data, tenant.name, tenant.namespace, now),
            None,
            _page_continues(data, len(batch) if isinstance(batch, list) else 0, page_size),
        )

    fetched = await asyncio.gather(*[fetch_one(tenant) for tenant in tenants])
    items: list[dict[str, Any]] = []
    errors: list[dict[str, str]] = []
    capped = False
    for tenant_items, error, page_full in fetched:
        items.extend(tenant_items)
        if error is not None:
            errors.append(error)
        capped = capped or page_full

    return {
        "items": items,
        "pagination": {
            "page": page,
            "pageSize": page_size,
            "totalItems": len(items),
            "totalPages": 1,
            "hasNextPage": capped,
            "capped": capped,
            "tenantErrors": errors,
        },
    }


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
    return await admin_lifecycle_request(tenant, "DELETE", f"/sandboxes/{sandbox_id}", request=request)


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


@router.get("/stats/runtime")
async def admin_runtime_stats(
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> dict[str, Any]:
    """Aggregate every sandbox, paging each tenant at the lifecycle maximum."""
    require_admin_session(payload)
    tenants = _selected_tenants(request)
    params = {
        key: value
        for key, value in request.query_params.items()
        if key not in {"tenant", "page", "pageSize", "page_size"}
    }
    client = LifecycleClient(get_settings())
    now = datetime.now(timezone.utc)

    async def fetch_all(tenant: Any) -> tuple[list[dict[str, Any]], dict[str, str] | None]:
        collected: list[dict[str, Any]] = []
        for page in range(1, _MAX_UPSTREAM_PAGES + 1):
            page_params = {
                **params,
                "page": str(page),
                "pageSize": str(_UPSTREAM_PAGE_SIZE),
            }
            try:
                resp = await client.request(tenant.api_key, "GET", "/sandboxes", params=page_params)
            except HTTPException as exc:
                return collected, _upstream_failure(tenant.name, exc)
            if resp.status_code >= 400:
                return collected, _response_failure(tenant.name, resp)
            data = resp.json()
            if not isinstance(data, dict):
                return collected, {
                    "tenant": tenant.name,
                    "code": "UPSTREAM_ERROR",
                    "message": "Invalid response",
                }
            batch = data.get("items") or []
            collected.extend(_enrich_items(data, tenant.name, tenant.namespace, now))
            batch_len = len(batch) if isinstance(batch, list) else 0
            if not _page_continues(data, batch_len, _UPSTREAM_PAGE_SIZE):
                return collected, None
        return collected, {
            "tenant": tenant.name,
            "code": "PAGE_LIMIT",
            "message": "Stopped after the upstream page cap",
        }

    fetched = await asyncio.gather(*[fetch_all(tenant) for tenant in tenants])
    items: list[dict[str, Any]] = []
    errors: list[dict[str, str]] = []
    for tenant_items, error in fetched:
        items.extend(tenant_items)
        if error is not None:
            errors.append(error)
    stats = aggregate_runtime_stats(items, now=now)
    stats["tenantErrors"] = errors
    return stats


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
