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

from app.config import Settings
from app.k8s_client import load_apps_v1


def _deployment_status(namespace: str, name: str) -> dict[str, Any]:
    apps = load_apps_v1()
    dep = apps.read_namespaced_deployment(name=name, namespace=namespace)
    spec_replicas = dep.spec.replicas or 0
    status = dep.status
    ready = status.ready_replicas or 0
    available = status.available_replicas or 0
    if spec_replicas == 0:
        st = "unknown"
        note = "Deployment has 0 desired replicas"
    elif ready >= spec_replicas:
        st = "ok"
        note = f"{ready}/{spec_replicas} replicas ready"
    elif ready == 0:
        st = "critical"
        note = f"0/{spec_replicas} replicas ready"
    else:
        st = "degraded"
        note = f"{ready}/{spec_replicas} replicas ready ({available} available)"
    return {
        "status": st,
        "note": note,
        "readyReplicas": ready,
        "desiredReplicas": spec_replicas,
        "deployment": name,
        "namespace": namespace,
    }


async def probe_platform_deployment(
    settings: Settings,
    *,
    component: str,
    deployment_name: str,
) -> dict[str, Any]:
    if not settings.bff_k8s_probe_enabled:
        return {
            "status": "disabled",
            "note": "Enable BFF_K8S_PROBE_ENABLED to probe Deployments",
        }
    if not deployment_name.strip():
        return {"status": "unknown", "note": f"No deployment name configured for {component}"}

    ns = settings.bff_k8s_system_namespace
    try:
        return await asyncio.to_thread(
            _deployment_status,
            ns,
            deployment_name.strip(),
        )
    except ApiException as exc:
        if exc.status == 404:
            return {
                "status": "not_found",
                "note": (
                    f'Deployment "{deployment_name}" not found in {ns}; '
                    "fix BFF_K8S_CONTROLLER_DEPLOYMENT / BFF_K8S_INGRESS_DEPLOYMENT"
                ),
                "deployment": deployment_name,
                "namespace": ns,
            }
        return {
            "status": "error",
            "note": exc.reason or str(exc),
            "deployment": deployment_name,
            "namespace": ns,
        }
    except Exception as exc:
        return {"status": "error", "note": str(exc), "deployment": deployment_name, "namespace": ns}
