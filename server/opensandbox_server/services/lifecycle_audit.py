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

"""Best-effort sandbox lifecycle audit writes (async, non-blocking for API latency)."""

from __future__ import annotations

import logging
import re
from datetime import datetime, timezone
from typing import Any

from fastapi import BackgroundTasks

from opensandbox_server.api.schema import CreateSandboxRequest, CreateSandboxResponse, Sandbox
from opensandbox_server.config import get_config
from opensandbox_server.repositories.lifecycle_audit.factory import get_lifecycle_audit_repository
from opensandbox_server.tenants.context import get_current_tenant

logger = logging.getLogger(__name__)

_MEMORY_RE = re.compile(
    r"^(\d+(?:\.\d+)?)\s*(Ki|Mi|Gi|Ti|K|M|G|T)?$",
    re.IGNORECASE,
)


def lifecycle_audit_enabled() -> bool:
    cfg = get_config()
    return cfg.store.type == "postgresql" and cfg.store.lifecycle_audit.enabled


def _tenant_context() -> tuple[str, str]:
    tenant = get_current_tenant()
    if tenant is not None:
        return tenant.name, tenant.namespace
    cfg = get_config()
    ns = ""
    if cfg.kubernetes is not None and cfg.kubernetes.namespace:
        ns = cfg.kubernetes.namespace
    return "", ns


def _image_uri_from_request(request: CreateSandboxRequest) -> str | None:
    if request.image is not None and request.image.uri:
        return str(request.image.uri)
    return None


def _image_uri_from_sandbox(sandbox: Sandbox | None) -> str | None:
    if sandbox is None or sandbox.image is None:
        return None
    uri = sandbox.image.uri
    return str(uri) if uri else None


def _limits_from_request(request: CreateSandboxRequest) -> tuple[str | None, str | None, float | None, float | None]:
    limits = request.resource_limits.root if request.resource_limits is not None else None
    if not limits:
        return None, None, None, None
    cpu = limits.get("cpu")
    memory = limits.get("memory")
    cpu_s = str(cpu) if cpu is not None else None
    mem_s = str(memory) if memory is not None else None
    return cpu_s, mem_s, _parse_cpu_cores(cpu_s), _parse_memory_gi(mem_s)


def _parse_cpu_cores(value: str | None) -> float | None:
    if not value:
        return None
    raw = value.strip()
    if raw.endswith("m"):
        try:
            return float(raw[:-1]) / 1000.0
        except ValueError:
            return None
    try:
        return float(raw)
    except ValueError:
        return None


def _parse_memory_gi(value: str | None) -> float | None:
    if not value:
        return None
    raw = value.strip()
    m = _MEMORY_RE.match(raw)
    if not m:
        try:
            return float(raw) / (1024**3)
        except ValueError:
            return None
    num = float(m.group(1))
    unit = (m.group(2) or "").upper()
    if unit in ("KI", "K"):
        return num * 1024 / (1024**3)
    if unit in ("MI", "M", ""):
        return num / 1024
    if unit in ("GI", "G"):
        return num
    if unit in ("TI", "T"):
        return num * 1024
    return num / 1024


def _ensure_utc(dt: datetime | None) -> datetime | None:
    if dt is None:
        return None
    if dt.tzinfo is None:
        return dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc)


def schedule_lifecycle_audit_create(
    background_tasks: BackgroundTasks,
    request: CreateSandboxRequest,
    response: CreateSandboxResponse,
) -> None:
    if not lifecycle_audit_enabled():
        return
    tenant_name, namespace = _tenant_context()
    cpu_limit, memory_limit, cpu_cores, memory_gi = _limits_from_request(request)
    background_tasks.add_task(
        _record_create,
        sandbox_id=response.id,
        tenant_name=tenant_name,
        namespace=namespace,
        state=response.status.state if response.status else None,
        image_uri=_image_uri_from_request(request),
        lifecycle_created_at=_ensure_utc(response.created_at),
        expires_at=_ensure_utc(response.expires_at),
        cpu_limit=cpu_limit,
        memory_limit=memory_limit,
        cpu_cores=cpu_cores,
        memory_gi=memory_gi,
    )


def schedule_lifecycle_audit_delete(
    background_tasks: BackgroundTasks,
    sandbox_id: str,
    final_sandbox: Sandbox | None,
) -> None:
    if not lifecycle_audit_enabled():
        return
    tenant_name, namespace = _tenant_context()
    if final_sandbox is not None:
        state = final_sandbox.status.state if final_sandbox.status else None
        background_tasks.add_task(
            _record_delete,
            sandbox_id=sandbox_id,
            tenant_name=tenant_name,
            namespace=namespace,
            final_state=state,
            image_uri=_image_uri_from_sandbox(final_sandbox),
            lifecycle_created_at=_ensure_utc(final_sandbox.created_at),
            expires_at=_ensure_utc(final_sandbox.expires_at),
        )
    else:
        background_tasks.add_task(
            _record_delete,
            sandbox_id=sandbox_id,
            tenant_name=tenant_name,
            namespace=namespace,
            final_state=None,
            image_uri=None,
            lifecycle_created_at=None,
            expires_at=None,
        )


def _record_create(**fields: Any) -> None:
    try:
        repo = get_lifecycle_audit_repository()
        if repo is None:
            return
        repo.upsert_created(**fields)
    except Exception:
        logger.exception("lifecycle audit create failed for sandbox %s", fields.get("sandbox_id"))


def _record_delete(**fields: Any) -> None:
    try:
        repo = get_lifecycle_audit_repository()
        if repo is None:
            return
        repo.mark_deleted(**fields)
    except Exception:
        logger.exception("lifecycle audit delete failed for sandbox %s", fields.get("sandbox_id"))
