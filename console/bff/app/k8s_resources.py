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
from typing import Any

from app.config import Settings
from app.k8s_client import load_core_v1
from app.tenants import load_tenants

SANDBOX_ID_LABEL = "opensandbox.io/id"


def _target_namespaces(settings: Settings, tenant_filter: str | None) -> list[tuple[str, str]]:
    tenants = load_tenants(settings.tenants_toml_path)
    if tenant_filter:
        tenants = [t for t in tenants if t.name == tenant_filter]
    pairs = [(t.name, t.namespace) for t in tenants]
    system_ns = settings.bff_k8s_system_namespace
    if not any(ns == system_ns for _, ns in pairs):
        pairs.append(("system", system_ns))
    return pairs


def _pod_row(pod: Any, tenant: str, namespace: str) -> dict[str, Any]:
    labels = pod.metadata.labels or {}
    status = pod.status
    phase = status.phase if status else "Unknown"
    ready = "Unknown"
    if status and status.container_statuses:
        ready = all(c.ready for c in status.container_statuses if c)
    return {
        "tenant": tenant,
        "namespace": namespace,
        "name": pod.metadata.name,
        "sandboxId": labels.get(SANDBOX_ID_LABEL),
        "phase": phase,
        "nodeName": pod.spec.node_name,
        "podIP": status.pod_ip if status else None,
        "startTime": pod.metadata.creation_timestamp.isoformat() if pod.metadata.creation_timestamp else None,
        "ready": ready,
        "labels": {k: v for k, v in labels.items() if k.startswith("opensandbox.io/") or k.startswith("app.kubernetes.io/")},
    }


def _list_workloads_sync(
    settings: Settings,
    *,
    tenant_filter: str | None,
    namespace_filter: str | None,
    sandbox_id: str | None,
    limit: int,
) -> list[dict[str, Any]]:
    v1 = load_core_v1()
    rows: list[dict[str, Any]] = []
    for tenant_name, namespace in _target_namespaces(settings, tenant_filter):
        if namespace_filter and namespace != namespace_filter:
            continue
        label_selector = None
        if sandbox_id:
            label_selector = f"{SANDBOX_ID_LABEL}={sandbox_id}"
        pods = v1.list_namespaced_pod(namespace=namespace, label_selector=label_selector, limit=limit)
        for pod in pods.items:
            if sandbox_id and (pod.metadata.labels or {}).get(SANDBOX_ID_LABEL) != sandbox_id:
                continue
            rows.append(_pod_row(pod, tenant_name, namespace))
            if len(rows) >= limit:
                return rows
    return rows


def _event_row(ev: Any, tenant: str, namespace: str) -> dict[str, Any]:
    obj = ev.involved_object
    return {
        "tenant": tenant,
        "namespace": namespace,
        "type": ev.type,
        "reason": ev.reason,
        "message": ev.message,
        "count": ev.count,
        "firstTimestamp": ev.first_timestamp.isoformat() if ev.first_timestamp else None,
        "lastTimestamp": ev.last_timestamp.isoformat() if ev.last_timestamp else None,
        "involvedObject": {
            "kind": obj.kind if obj else None,
            "name": obj.name if obj else None,
        },
    }


def _list_events_sync(
    settings: Settings,
    *,
    tenant_filter: str | None,
    namespace_filter: str | None,
    sandbox_id: str | None,
    limit: int,
) -> list[dict[str, Any]]:
    v1 = load_core_v1()
    rows: list[dict[str, Any]] = []
    for tenant_name, namespace in _target_namespaces(settings, tenant_filter):
        if namespace_filter and namespace != namespace_filter:
            continue
        field_selector = None
        if sandbox_id:
            # Events don't index by sandbox label; filter client-side on pod name prefix if needed
            pass
        events = v1.list_namespaced_event(namespace=namespace, field_selector=field_selector, limit=limit)
        for ev in sorted(events.items, key=lambda e: e.last_timestamp or e.event_time or e.metadata.creation_timestamp, reverse=True):
            if sandbox_id:
                obj = ev.involved_object
                if obj and obj.kind == "Pod":
                    try:
                        pod = v1.read_namespaced_pod(name=obj.name, namespace=namespace)
                        if (pod.metadata.labels or {}).get(SANDBOX_ID_LABEL) != sandbox_id:
                            continue
                    except Exception:
                        continue
                else:
                    continue
            rows.append(_event_row(ev, tenant_name, namespace))
            if len(rows) >= limit:
                return rows
    return rows


async def list_workloads(
    settings: Settings,
    *,
    tenant: str | None = None,
    namespace: str | None = None,
    sandbox_id: str | None = None,
    limit: int = 200,
) -> dict[str, Any]:
    if not settings.bff_k8s_probe_enabled:
        return {
            "items": [],
            "disabled": True,
            "message": "Set BFF_K8S_PROBE_ENABLED=true and configure RBAC",
        }
    items = await asyncio.to_thread(
        _list_workloads_sync,
        settings,
        tenant_filter=tenant,
        namespace_filter=namespace,
        sandbox_id=sandbox_id,
        limit=min(limit, 500),
    )
    return {"items": items, "totalItems": len(items)}


async def list_events(
    settings: Settings,
    *,
    tenant: str | None = None,
    namespace: str | None = None,
    sandbox_id: str | None = None,
    limit: int = 100,
) -> dict[str, Any]:
    if not settings.bff_k8s_probe_enabled:
        return {
            "items": [],
            "disabled": True,
            "message": "Set BFF_K8S_PROBE_ENABLED=true and configure RBAC",
        }
    items = await asyncio.to_thread(
        _list_events_sync,
        settings,
        tenant_filter=tenant,
        namespace_filter=namespace,
        sandbox_id=sandbox_id,
        limit=min(limit, 300),
    )
    return {"items": items, "totalItems": len(items)}
