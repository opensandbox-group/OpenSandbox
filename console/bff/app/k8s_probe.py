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

import httpx

from app.config import Settings
from app.k8s_client import load_core_v1


async def _probe_pod_readyz(ip: str, port: int, timeout: float) -> dict[str, Any]:
    url = f"http://{ip}:{port}/readyz"
    try:
        async with httpx.AsyncClient(timeout=timeout) as http:
            resp = await http.get(url)
            body = resp.json() if resp.headers.get("content-type", "").startswith("application/json") else {}
            return {
                "ready": resp.status_code == 200 and bool(body.get("ready", resp.status_code == 200)),
                "reasons": body.get("reasons") if isinstance(body.get("reasons"), list) else [],
                "httpStatus": resp.status_code,
            }
    except Exception as exc:
        return {"ready": False, "reasons": ["probe-failed"], "error": str(exc)}


async def probe_node_agent(settings: Settings) -> dict[str, Any]:
    if not settings.bff_k8s_probe_enabled:
        return {
            "status": "disabled",
            "note": "Enable BFF_K8S_PROBE_ENABLED and grant BFF pod list/get pods in node-agent namespace",
        }

    def list_pods() -> list[dict[str, str]]:
        v1 = load_core_v1()
        pods = v1.list_namespaced_pod(
            namespace=settings.bff_k8s_node_agent_namespace,
            label_selector=settings.bff_k8s_node_agent_label_selector,
        )
        out: list[dict[str, str]] = []
        for pod in pods.items:
            ip = pod.status.pod_ip
            if not ip:
                continue
            out.append({"name": pod.metadata.name, "node": pod.spec.node_name or "", "ip": ip})
        return out

    try:
        pod_refs = await asyncio.to_thread(list_pods)
    except RuntimeError as exc:
        return {"status": "unknown", "note": str(exc)}
    except Exception as exc:
        return {"status": "error", "note": str(exc), "readyPods": 0, "totalPods": 0}

    if not pod_refs:
        return {
            "status": "missing",
            "note": f"No node-agent pods in {settings.bff_k8s_node_agent_namespace}",
            "readyPods": 0,
            "totalPods": 0,
        }

    probes = await asyncio.gather(
        *[
            _probe_pod_readyz(p["ip"], settings.bff_k8s_node_agent_probe_port, settings.bff_http_timeout_seconds)
            for p in pod_refs
        ],
    )
    ready_count = sum(1 for p in probes if p.get("ready"))
    sample_reasons: list[str] = []
    for p in probes:
        for r in p.get("reasons") or []:
            if r not in sample_reasons:
                sample_reasons.append(str(r))

    if ready_count == len(pod_refs):
        status = "ok"
    elif ready_count == 0:
        status = "critical"
    else:
        status = "degraded"

    return {
        "status": status,
        "note": f"{ready_count}/{len(pod_refs)} DaemonSet pods ready on /readyz",
        "readyPods": ready_count,
        "totalPods": len(pod_refs),
        "sampleReasons": sample_reasons[:8],
        "pods": [
            {**pod_refs[i], "probe": probes[i]} for i in range(len(pod_refs))
        ],
    }
