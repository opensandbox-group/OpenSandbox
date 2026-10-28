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

from kubernetes.client.exceptions import ApiException
from kubernetes.config.config_exception import ConfigException

from app.config import Settings
from app.k8s_client import load_core_v1
from app.tenants import load_tenants

SANDBOX_ID_LABEL = "opensandbox.io/id"


def primary_pod_name_for_sandbox(sandbox_id: str) -> str:
    """BatchSandbox / fast-sandbox Pod 名：`<sandboxId>-0`。"""
    sid = sandbox_id.strip()
    if sid.endswith("-0"):
        return sid
    return f"{sid}-0"


def pod_name_candidates_for_sandbox(sandbox_id: str) -> list[str]:
    sid = sandbox_id.strip()
    if not sid:
        return []
    primary = primary_pod_name_for_sandbox(sid)
    if primary == sid:
        return [sid]
    return [primary, sid]


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
        matched = 0
        for pod in pods.items:
            if sandbox_id and (pod.metadata.labels or {}).get(SANDBOX_ID_LABEL) != sandbox_id:
                continue
            rows.append(_pod_row(pod, tenant_name, namespace))
            matched += 1
            if len(rows) >= limit:
                return rows
        # Pod 名多为 `<sandboxId>-0`，且未必带 opensandbox.io/id 标签
        if sandbox_id and matched == 0:
            for pod_name in pod_name_candidates_for_sandbox(sandbox_id):
                try:
                    pod = v1.read_namespaced_pod(name=pod_name, namespace=namespace)
                except Exception:
                    continue
                rows.append(_pod_row(pod, tenant_name, namespace))
                if len(rows) >= limit:
                    return rows
                break
    return rows


def _k8s_list_error(exc: Exception) -> dict[str, Any]:
    if isinstance(exc, ConfigException):
        return {
            "items": [],
            "message": str(exc),
            "errorCode": "K8S_CONFIG",
        }
    if isinstance(exc, ApiException):
        body = exc.body if isinstance(exc.body, str) else str(exc.body or exc.reason or exc)
        return {
            "items": [],
            "message": body,
            "errorCode": "K8S_API_ERROR",
        }
    return {
        "items": [],
        "message": str(exc),
        "errorCode": "K8S_ERROR",
    }


def _event_sort_key(ev: Any) -> Any:
    return ev.last_timestamp or ev.event_time or ev.metadata.creation_timestamp


def _list_namespace_events(v1: Any, namespace: str, *, page_size: int, max_pages: int) -> list[Any]:
    collected: list[Any] = []
    continue_token: str | None = None
    for _ in range(max_pages):
        resp = v1.list_namespaced_event(
            namespace=namespace,
            limit=page_size,
            _continue=continue_token,
        )
        collected.extend(resp.items)
        continue_token = resp.metadata._continue
        if not continue_token:
            break
    collected.sort(key=_event_sort_key, reverse=True)
    return collected


def _pod_names_for_sandbox(v1: Any, namespace: str, sandbox_id: str) -> set[str]:
    names = set(pod_name_candidates_for_sandbox(sandbox_id))
    try:
        pods = v1.list_namespaced_pod(
            namespace=namespace,
            label_selector=f"{SANDBOX_ID_LABEL}={sandbox_id}",
        )
        for pod in pods.items:
            if pod.metadata.name:
                names.add(pod.metadata.name)
    except ApiException:
        raise
    except Exception:
        pass
    return names


def _event_matches_sandbox(ev: Any, pod_names: set[str]) -> bool:
    obj = ev.involved_object
    if not obj or obj.kind != "Pod" or not obj.name:
        return False
    return obj.name in pod_names


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
    page_size = min(max(limit, 50), 300)
    max_pages = 8
    for tenant_name, namespace in _target_namespaces(settings, tenant_filter):
        if namespace_filter and namespace != namespace_filter:
            continue
        pod_names: set[str] | None = None
        if sandbox_id:
            pod_names = _pod_names_for_sandbox(v1, namespace, sandbox_id)
        events = _list_namespace_events(v1, namespace, page_size=page_size, max_pages=max_pages)
        for ev in events:
            if pod_names is not None and not _event_matches_sandbox(ev, pod_names):
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
    capped = min(limit, 500)
    try:
        items = await asyncio.to_thread(
            _list_workloads_sync,
            settings,
            tenant_filter=tenant,
            namespace_filter=namespace,
            sandbox_id=sandbox_id,
            limit=capped,
        )
    except (ConfigException, ApiException, RuntimeError) as exc:
        return {**_k8s_list_error(exc), "totalItems": 0}
    except Exception as exc:
        return {**_k8s_list_error(exc), "totalItems": 0}
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
    capped = min(limit, 300)
    try:
        items = await asyncio.to_thread(
            _list_events_sync,
            settings,
            tenant_filter=tenant,
            namespace_filter=namespace,
            sandbox_id=sandbox_id,
            limit=capped,
        )
    except (ConfigException, ApiException, RuntimeError) as exc:
        return {**_k8s_list_error(exc), "totalItems": 0}
    except Exception as exc:
        return {**_k8s_list_error(exc), "totalItems": 0}
    return {"items": items, "totalItems": len(items)}
