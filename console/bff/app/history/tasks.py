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

from app.config import Settings, get_settings
from app.history import store

logger = logging.getLogger(__name__)


def _tenant_namespace(payload: dict[str, Any]) -> tuple[str, str]:
    if payload.get("role") == "admin":
        return str(payload.get("tenant") or ""), str(payload.get("namespace") or "")
    return str(payload.get("tenant") or ""), str(payload.get("namespace") or "")


def schedule_upsert(
    payload: dict[str, Any],
    sandbox: dict[str, Any],
    *,
    source: str = "console",
    create_request: dict[str, Any] | None = None,
) -> None:
    settings = get_settings()
    if not settings.bff_history_enabled:
        return
    tenant = sandbox.get("tenant") or payload.get("tenant")
    namespace = sandbox.get("namespace") or payload.get("namespace")
    if not tenant or not namespace:
        tenant, namespace = _tenant_namespace(payload)
    if not tenant:
        return

    async def _run() -> None:
        try:
            await asyncio.to_thread(
                store.upsert_from_sandbox,
                settings,
                tenant_name=str(tenant),
                namespace=str(namespace),
                sandbox=sandbox,
                source=source,
                create_request=create_request,
            )
        except Exception:
            logger.exception("history upsert failed for sandbox %s", sandbox.get("id"))

    asyncio.create_task(_run())


def schedule_mark_deleted(
    sandbox_id: str,
    *,
    tenant: str,
    namespace: str,
    final_sandbox: dict[str, Any] | None = None,
) -> None:
    settings = get_settings()
    if not settings.bff_history_enabled:
        return

    async def _run() -> None:
        try:
            await asyncio.to_thread(
                store.mark_deleted,
                settings,
                sandbox_id,
                tenant_name=tenant,
                namespace=namespace,
                final_sandbox=final_sandbox,
            )
        except Exception:
            logger.exception("history mark_deleted failed for sandbox %s", sandbox_id)

    asyncio.create_task(_run())
