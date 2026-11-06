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
from contextvars import ContextVar
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any, Callable

from fastapi import BackgroundTasks, HTTPException

from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    CreateSandboxResponse,
    ResourceLimits,
    Sandbox,
)
from opensandbox_server.config import get_config
from opensandbox_server.repositories.lifecycle_audit.factory import get_lifecycle_audit_repository
from opensandbox_server.tenants.context import get_current_tenant

logger = logging.getLogger(__name__)


@dataclass(frozen=True, slots=True)
class LifecycleAuditShape:
    """Image and resource limits the runtime actually scheduled."""

    image_uri: str | None
    resource_limits: ResourceLimits | None


_audit_shape: ContextVar[LifecycleAuditShape | None] = ContextVar(
    "lifecycle_audit_shape",
    default=None,
)


def publish_lifecycle_audit_shape(
    *,
    image_uri: str | None,
    resource_limits: ResourceLimits | None,
) -> None:
    """Remember the scheduled shape until the create route records the audit."""
    _audit_shape.set(
        LifecycleAuditShape(image_uri=image_uri, resource_limits=resource_limits)
    )


def take_lifecycle_audit_shape() -> LifecycleAuditShape | None:
    shape = _audit_shape.get()
    _audit_shape.set(None)
    return shape


def shape_from_pool_manifest(pool: dict[str, Any]) -> LifecycleAuditShape:
    """Read the sandbox container from a Pool CR. Caller limits are not used."""
    template = (pool.get("spec") or {}).get("template") or {}
    spec = template.get("spec") or {}
    containers = spec.get("containers") or []
    container: dict[str, Any] | None = None
    if isinstance(containers, list):
        for item in containers:
            if isinstance(item, dict) and item.get("name") == "sandbox":
                container = item
                break
        if container is None:
            container = next((item for item in containers if isinstance(item, dict)), None)
    image_uri = None
    resource_limits = None
    if isinstance(container, dict):
        raw_image = container.get("image")
        image_uri = str(raw_image) if raw_image else None
        raw_limits = (container.get("resources") or {}).get("limits") or {}
        if isinstance(raw_limits, dict):
            root: dict[str, str] = {}
            if raw_limits.get("cpu") is not None:
                root["cpu"] = str(raw_limits["cpu"])
            if raw_limits.get("memory") is not None:
                root["memory"] = str(raw_limits["memory"])
            if root:
                resource_limits = ResourceLimits(root=root)
    return LifecycleAuditShape(image_uri=image_uri, resource_limits=resource_limits)


def shape_from_template_spec(
    source_image: str | None,
    resource_limits: dict[str, Any] | None,
) -> LifecycleAuditShape:
    """Guest shape baked into a template. Defaults match the template CRD."""
    from opensandbox_server.services.templates.template_service import (
        DEFAULT_MEMORY,
        DEFAULT_VCPU,
    )

    limits = resource_limits or {}
    return LifecycleAuditShape(
        image_uri=source_image or None,
        resource_limits=ResourceLimits(
            root={
                "cpu": str(limits.get("cpu") or DEFAULT_VCPU),
                "memory": str(limits.get("memory") or DEFAULT_MEMORY),
            }
        ),
    )


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


def _limits_from_resource_limits(
    limits: ResourceLimits | None,
) -> tuple[str | None, str | None, float | None, float | None]:
    root = limits.root if limits is not None else None
    if not root:
        return None, None, None, None
    cpu = root.get("cpu")
    memory = root.get("memory")
    cpu_s = str(cpu) if cpu is not None else None
    mem_s = str(memory) if memory is not None else None
    return cpu_s, mem_s, _parse_cpu_cores(cpu_s), _parse_memory_gi(mem_s)


def _limits_from_request(request: CreateSandboxRequest) -> tuple[str | None, str | None, float | None, float | None]:
    return _limits_from_resource_limits(request.resource_limits)


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
    # Integer quantities, including a bare byte count, follow parse_memory_limit.
    from opensandbox_server.services.helpers import parse_memory_limit

    nbytes = parse_memory_limit(raw)
    if nbytes is not None:
        return nbytes / (1024**3)
    m = _MEMORY_RE.match(raw)
    if not m:
        return None
    num = float(m.group(1))
    unit = (m.group(2) or "").upper()
    if unit in ("KI", "K"):
        return num * 1024 / (1024**3)
    if unit in ("MI", "M"):
        return num / 1024
    if unit == "":
        return num / (1024**3)
    if unit in ("GI", "G"):
        return num
    if unit in ("TI", "T"):
        return num * 1024
    return num / (1024**3)


def _audit_metadata(metadata: dict[str, str] | None) -> dict[str, str] | None:
    """Sandbox metadata is the label map stored on lifecycle history rows."""
    if not metadata:
        return None
    labels: dict[str, str] = {}
    for key, value in metadata.items():
        if value is None:
            continue
        name = str(key).strip()
        if name:
            labels[name] = str(value)
    return labels or None


def _ensure_utc(dt: datetime | None) -> datetime | None:
    if dt is None:
        return None
    if dt.tzinfo is None:
        return dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc)


def load_sandbox_for_audit_delete(load: Callable[[], Sandbox]) -> Sandbox | None:
    """Read the pre-delete sandbox only when audit will store it."""
    if not lifecycle_audit_enabled():
        return None
    try:
        return load()
    except HTTPException:
        return None


def schedule_lifecycle_audit_create(
    background_tasks: BackgroundTasks,
    request: CreateSandboxRequest,
    response: CreateSandboxResponse,
) -> None:
    # Always consume a published shape so it cannot leak into a later request.
    shape = take_lifecycle_audit_shape()
    if not lifecycle_audit_enabled():
        return
    tenant_name, namespace = _tenant_context()
    if shape is not None:
        image_uri = shape.image_uri
        cpu_limit, memory_limit, cpu_cores, memory_gi = _limits_from_resource_limits(
            shape.resource_limits
        )
    else:
        image_uri = _image_uri_from_request(request)
        cpu_limit, memory_limit, cpu_cores, memory_gi = _limits_from_request(request)
    background_tasks.add_task(
        _record_create,
        sandbox_id=response.id,
        tenant_name=tenant_name,
        namespace=namespace,
        state=response.status.state if response.status else None,
        image_uri=image_uri,
        lifecycle_created_at=_ensure_utc(response.created_at),
        expires_at=_ensure_utc(response.expires_at),
        cpu_limit=cpu_limit,
        memory_limit=memory_limit,
        cpu_cores=cpu_cores,
        memory_gi=memory_gi,
        metadata=_audit_metadata(request.metadata or response.metadata),
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
            metadata=_audit_metadata(final_sandbox.metadata),
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
