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
from app.deps import get_session_payload, require_admin_session, require_tenant_session
from app.history import reconcile, store

router = APIRouter(prefix="/history", tags=["history"])


def _history_disabled() -> None:
    if not get_settings().bff_history_enabled:
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail={
                "code": "HISTORY_DISABLED",
                "message": "Set BFF_HISTORY_ENABLED and BFF_HISTORY_DATABASE_URL",
            },
        )


@router.get("/sandboxes")
async def list_sandbox_history(
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> dict[str, Any]:
    _history_disabled()
    page = max(1, int(request.query_params.get("page", 1) or 1))
    page_size = min(200, max(1, int(request.query_params.get("pageSize", 20) or 20)))
    include_active = request.query_params.get("includeActive", "true").lower() != "false"
    settings = get_settings()

    if payload.get("role") == "admin":
        require_admin_session(payload)
        tenant_filter = request.query_params.get("tenant")
    else:
        require_tenant_session(payload)
        tenant_filter = str(payload.get("tenant"))

    if (
        request.query_params.get("reconcile", "true").lower() != "false"
        and settings.bff_history_reconcile_on_read
    ):
        tenants = reconcile.tenants_for_reconcile(settings, payload, tenant_filter)
        await reconcile.sync_with_lifecycle(settings, tenants=tenants)

    period_from: datetime | None = None
    period_to: datetime | None = None
    raw_from = request.query_params.get("from")
    raw_to = request.query_params.get("to")
    if raw_from and raw_to:
        period_from = _parse_period_param(raw_from, "from")
        period_to = _parse_period_param(raw_to, "to")
        if period_to <= period_from:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={"code": "INVALID_PERIOD", "message": "to must be after from"},
            )

    return await asyncio.to_thread(
        store.list_history,
        settings,
        tenant_name=tenant_filter,
        page=page,
        page_size=page_size,
        include_active=include_active,
        period_from=period_from,
        period_to=period_to,
    )


@router.get("/sandboxes/{sandbox_id}")
async def get_sandbox_history_record(
    sandbox_id: str,
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> dict[str, Any]:
    _history_disabled()
    settings = get_settings()
    if payload.get("role") == "admin":
        require_admin_session(payload)
        tenant_filter = request.query_params.get("tenant")
        if not tenant_filter or not tenant_filter.strip():
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={"code": "MISSING_TENANT", "message": "Admin must pass query parameter tenant="},
            )
        tenant_filter = tenant_filter.strip()
    else:
        require_tenant_session(payload)
        tenant_filter = str(payload.get("tenant"))

    record = await asyncio.to_thread(
        store.get_history_record,
        settings,
        sandbox_id=sandbox_id,
        tenant_name=tenant_filter,
    )
    if not record:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail={"code": "HISTORY_NOT_FOUND", "message": "No history record for this sandbox"},
        )
    return record


@router.get("/stats")
async def history_statistics(
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> dict[str, Any]:
    _history_disabled()
    settings = get_settings()
    if payload.get("role") == "admin":
        require_admin_session(payload)
        tenant_filter = request.query_params.get("tenant")
    else:
        require_tenant_session(payload)
        tenant_filter = str(payload.get("tenant"))

    if (
        request.query_params.get("reconcile", "true").lower() != "false"
        and settings.bff_history_reconcile_on_read
    ):
        tenants = reconcile.tenants_for_reconcile(settings, payload, tenant_filter)
        await reconcile.sync_with_lifecycle(settings, tenants=tenants)

    return await asyncio.to_thread(store.history_stats, settings, tenant_name=tenant_filter)


@router.get("/images/stats")
async def image_statistics(
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> dict[str, Any]:
    _history_disabled()
    settings = get_settings()
    if payload.get("role") == "admin":
        require_admin_session(payload)
        tenant_filter = request.query_params.get("tenant")
    else:
        require_tenant_session(payload)
        tenant_filter = str(payload.get("tenant"))

    period_from: datetime | None = None
    period_to: datetime | None = None
    raw_from = request.query_params.get("from")
    raw_to = request.query_params.get("to")
    if raw_from and raw_from.strip():
        period_from = _parse_period_param(raw_from, "from")
    if raw_to and raw_to.strip():
        period_to = _parse_period_param(raw_to, "to")
    if period_from and period_to and period_from >= period_to:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={"code": "INVALID_PERIOD", "message": "from must be before to"},
        )

    if (
        request.query_params.get("reconcile", "true").lower() != "false"
        and settings.bff_history_reconcile_on_read
    ):
        tenants = reconcile.tenants_for_reconcile(settings, payload, tenant_filter)
        await reconcile.sync_with_lifecycle(settings, tenants=tenants)

    return await asyncio.to_thread(
        store.image_stats,
        settings,
        tenant_name=tenant_filter,
        period_from=period_from,
        period_to=period_to,
    )


def _parse_period_param(value: str | None, name: str) -> datetime:
    if not value or not value.strip():
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={"code": "INVALID_PERIOD", "message": f"Missing query parameter: {name}"},
        )
    normalized = value.strip().replace("Z", "+00:00")
    try:
        dt = datetime.fromisoformat(normalized)
    except ValueError as exc:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={"code": "INVALID_PERIOD", "message": f"Invalid {name}: {value}"},
        ) from exc
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc)


@router.get("/usage")
async def usage_allocation(
    request: Request,
    payload: dict = Depends(get_session_payload),
) -> dict[str, Any]:
    _history_disabled()
    settings = get_settings()
    period_from = _parse_period_param(request.query_params.get("from"), "from")
    period_to = _parse_period_param(request.query_params.get("to"), "to")

    viewer_tenant_only: str | None = None
    display_tenant: str | None = None
    if payload.get("role") == "admin":
        require_admin_session(payload)
        display_tenant = request.query_params.get("tenant")
    else:
        require_tenant_session(payload)
        viewer_tenant_only = str(payload.get("tenant"))

    if (
        request.query_params.get("reconcile", "true").lower() != "false"
        and settings.bff_history_reconcile_on_read
    ):
        tenants = reconcile.tenants_for_reconcile(settings, payload, display_tenant or viewer_tenant_only)
        await reconcile.sync_with_lifecycle(settings, tenants=tenants)

    try:
        return await asyncio.to_thread(
            store.usage_allocation,
            settings,
            period_from=period_from,
            period_to=period_to,
            display_tenant=display_tenant,
            viewer_tenant_only=viewer_tenant_only,
        )
    except ValueError as exc:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={"code": "INVALID_PERIOD", "message": str(exc)},
        ) from exc
