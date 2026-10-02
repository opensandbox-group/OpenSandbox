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
import logging
from typing import Any

from app.config import Settings
from app.history import store
from app.lifecycle import LifecycleClient
from app.runtime import attach_runtime_summary
from app.tenants import TenantRecord, load_tenants

logger = logging.getLogger(__name__)

_MAX_PAGES = 10
_PAGE_SIZE = 200


async def _live_sandboxes_for_tenant(
    settings: Settings,
    tenant: TenantRecord,
) -> list[dict[str, Any]] | None:
    """Paginated Lifecycle list for one tenant; None if upstream failed."""
    client = LifecycleClient(settings)
    items: list[dict[str, Any]] = []
    page = 1
    while page <= _MAX_PAGES:
        resp = await client.request(
            tenant.api_key,
            "GET",
            "/sandboxes",
            params={"page": page, "pageSize": _PAGE_SIZE},
        )
        if resp.status_code >= 400:
            logger.warning(
                "history reconcile skipped for tenant %s: lifecycle %s",
                tenant.name,
                resp.status_code,
            )
            return None
        try:
            data = resp.json()
        except Exception:
            return None
        if not isinstance(data, dict):
            return None
        for item in data.get("items") or []:
            if isinstance(item, dict) and item.get("id"):
                items.append(item)
        pagination = data.get("pagination") or {}
        if not pagination.get("hasNextPage"):
            break
        page += 1
    return items


def _sync_tenant_history(
    settings: Settings,
    tenant: TenantRecord,
    live_items: list[dict[str, Any]],
) -> None:
    live_ids: set[str] = set()
    for item in live_items:
        sid = item.get("id")
        if not sid:
            continue
        live_ids.add(str(sid))
        enriched = attach_runtime_summary(item)
        store.upsert_from_sandbox(
            settings,
            tenant_name=tenant.name,
            namespace=tenant.namespace,
            sandbox=enriched,
            source="lifecycle-reconcile",
        )
    store.mark_missing_as_terminated(
        settings,
        tenant_name=tenant.name,
        live_sandbox_ids=live_ids,
    )


async def sync_with_lifecycle(
    settings: Settings,
    *,
    tenants: list[TenantRecord],
) -> None:
    """Upsert live Lifecycle sandboxes into history, then close rows no longer listed."""
    if not settings.bff_history_enabled:
        return

    async def _one(tenant: TenantRecord) -> None:
        live_items = await _live_sandboxes_for_tenant(settings, tenant)
        if live_items is None:
            return
        await asyncio.to_thread(_sync_tenant_history, settings, tenant, live_items)

    await asyncio.gather(*[_one(t) for t in tenants])


def tenants_for_reconcile(
    settings: Settings,
    payload: dict[str, Any],
    tenant_filter: str | None,
) -> list[TenantRecord]:
    all_tenants = load_tenants(settings.tenants_toml_path)
    if payload.get("role") == "admin":
        if tenant_filter:
            return [t for t in all_tenants if t.name == tenant_filter]
        return all_tenants
    name = str(payload.get("tenant") or "")
    return [t for t in all_tenants if t.name == name]
