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
"""Export effective workload configuration without copying runtime identities."""

import asyncio
from typing import Any, NoReturn

import httpx
from fastapi import HTTPException

from opensandbox_server.api.schema import CreateSandboxRequest
from opensandbox_server.api.fork_schema import ForkSandboxRequest
from opensandbox_server.services.constants import (
    ALLOWED_EGRESS_ENV_VARS, OPENSANDBOX_RUNTIME_VOLUME_NAME, SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY,
    SANDBOX_SECURE_ACCESS_TOKEN_METADATA_KEY,
)
from opensandbox_server.services.snapshot_restore import DEFAULT_SNAPSHOT_RESTORE_ENTRYPOINT
from opensandbox_server.services.validators import ensure_timeout_within_limit


def reject(message: str, code: str = "UNSUPPORTED_CONFIG", status: int = 409) -> NoReturn:
    raise HTTPException(status, detail={"code": f"FORK::{code}", "message": message})


def user_env(entries: list[str]) -> dict[str, str]:
    result = {}
    for entry in entries:
        key, _, value = entry.partition("=")
        if not key.startswith("OPENSANDBOX_") and key != "EXECD_INIT":
            result[key] = value
    return result


def export_docker(service, sandbox_id: str) -> tuple[dict, bool]:
    container = service._get_container_by_sandbox_id(sandbox_id)
    attrs = container.attrs
    config = attrs.get("Config") or {}
    if config.get("WorkingDir") not in {None, "", "/"}:
        reject("Custom working directories cannot yet be inherited safely.")
    host = attrs.get("HostConfig") or {}
    platform = service._resolve_platform_for_container(container, config.get("Labels") or {}, True)
    if platform is None or platform.os != "linux":
        reject("Fork requires a Linux container rootfs.", "UNSUPPORTED_RUNTIME", 501)
    if any(mount.get("Destination") != "/opt/opensandbox" for mount in attrs.get("Mounts") or []):
        reject("Fork does not copy or share user volumes.", "VOLUMES_UNSUPPORTED")
    if host.get("DeviceRequests") or host.get("CpuQuota") or host.get("CpusetCpus"):
        reject("This container uses resource constraints that cannot be reconstructed safely.")
    limits = {}
    if host.get("Memory"):
        limits["memory"] = str(host["Memory"])
    if host.get("NanoCpus"):
        limits["cpu"] = str(host["NanoCpus"] / 1_000_000_000)
    labels = config.get("Labels") or {}
    env = config.get("Env") or []
    inherited_env = user_env(env)
    if labels.get(SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY):
        sidecar = service.docker_client.containers.get(f"sandbox-egress-{sandbox_id}")
        for entry in (sidecar.attrs.get("Config") or {}).get("Env") or []:
            key, _, value = entry.partition("=")
            if key in ALLOWED_EGRESS_ENV_VARS and key != "OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT":
                inherited_env[key] = value
    return {
        "env": inherited_env, "resourceLimits": limits,
        "platform": platform.model_dump(),
        "credentialProxy": {"enabled": "OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true" in env},
        "secureAccess": False,
    }, bool(labels.get(SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY))


def export_kubernetes(service, sandbox_id: str) -> tuple[dict, bool]:
    from kubernetes.client import ApiClient
    from opensandbox_server.services.k8s.batchsandbox_provider import BatchSandboxProvider
    from opensandbox_server.services.k8s.workload_access import _get_owned_workload_or_404
    from opensandbox_server.services.k8s.snapshot_runtime import _BATCHSANDBOX_NAME_LABEL

    if not isinstance(service.workload_provider, BatchSandboxProvider):
        reject("Fork currently requires the BatchSandbox rootfs backend.", "UNSUPPORTED_RUNTIME", 501)
    k8s_config = getattr(getattr(service, "app_config", None), "kubernetes", None)
    if k8s_config and k8s_config.batchsandbox_template_file:
        reject("Fork cannot safely reconstruct a custom BatchSandbox template.", "UNSUPPORTED_RUNTIME", 501)
    ns = service._resolve_namespace_for_lookup(sandbox_id)
    workload = _get_owned_workload_or_404(service.workload_provider, ns, sandbox_id)
    spec = workload.get("spec") or {}
    if spec.get("poolRef") or spec.get("replicas", 1) != 1:
        reject("Fork does not support pooled or multi-replica workloads.", "UNSUPPORTED_RUNTIME", 501)
    pods = service.k8s_client.list_pods(namespace=ns, label_selector=f"{_BATCHSANDBOX_NAME_LABEL}={sandbox_id}")
    pod_values: list[dict[str, Any]] = []
    for pod in pods:
        value = pod if isinstance(pod, dict) else ApiClient().sanitize_for_serialization(pod)
        if not isinstance(value, dict):
            reject("Cannot inspect the source Pod.", "RUNTIME_PREFLIGHT_FAILED")
        pod_values.append(value)
    running = next((pod for pod in pod_values if (pod.get("status") or {}).get("phase") == "Running"), None)
    if running is None:
        reject("Cannot inspect a running source Pod.", "RUNTIME_PREFLIGHT_FAILED")
    pod_spec = running.get("spec") or {}
    containers = pod_spec.get("containers") or []
    user = next((c for c in containers if c.get("name") == "sandbox"), None)
    if user is None:
        reject("Cannot identify the user container.")
    internal_volumes = {OPENSANDBOX_RUNTIME_VOLUME_NAME}
    for volume in pod_spec.get("volumes") or []:
        if (volume.get("name", "").startswith("kube-api-access-")
                and any("serviceAccountToken" in source for source in (volume.get("projected") or {}).get("sources") or [])):
            internal_volumes.add(volume["name"])
    if any(m.get("name") not in internal_volumes for m in user.get("volumeMounts") or []):
        reject("Fork does not copy or share user volumes.", "VOLUMES_UNSUPPORTED")
    if user.get("envFrom") or any("valueFrom" in e for e in user.get("env") or []):
        reject("Fork does not resolve Secret, ConfigMap or field-based environment sources.")
    if pod_spec.get("runtimeClassName") or user.get("workingDir"):
        reject("Custom runtime classes and working directories cannot yet be inherited safely.")
    annotations = (workload.get("metadata") or {}).get("annotations") or {}
    resources = user.get("resources") or {}
    egress = next((c for c in containers if c.get("name") == "egress"), {})
    proxy = any(e.get("name") == "OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT" and e.get("value") == "true"
                for e in egress.get("env") or [])
    inherited_env = user_env([f"{e['name']}={e.get('value', '')}" for e in user.get("env") or []])
    for entry in egress.get("env") or []:
        if entry.get("name") in ALLOWED_EGRESS_ENV_VARS and entry["name"] != "OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT":
            if "valueFrom" in entry:
                reject("Cannot inherit a Secret-based egress setting.")
            inherited_env[entry["name"]] = entry.get("value", "")
    return {
        "env": inherited_env,
        "resourceLimits": resources.get("limits") or {},
        "resourceRequests": resources.get("requests") or {},
        "credentialProxy": {"enabled": proxy},
        "secureAccess": bool(annotations.get(SANDBOX_SECURE_ACCESS_TOKEN_METADATA_KEY)),
    }, bool(egress)


async def build_fork_config(service, sandbox_id: str, request: ForkSandboxRequest) -> dict:
    sandbox = await asyncio.to_thread(service.get_sandbox, sandbox_id)
    if sandbox.status.state != "Running":
        reject("Fork requires a Running source sandbox.", "INVALID_SOURCE_STATE")
    if sandbox_id.startswith("fsb-") or sandbox.allocation is not None:
        reject("Fork does not support FastSandbox or pool allocations.", "UNSUPPORTED_RUNTIME", 501)
    backend: Any = getattr(service, "_kubernetes", service)
    ensure_timeout_within_limit(request.timeout, backend.app_config.server.max_sandbox_timeout_seconds)
    if hasattr(backend, "docker_client"):
        values, has_egress = await asyncio.to_thread(export_docker, backend, sandbox_id)
    elif hasattr(backend, "workload_provider"):
        values, has_egress = await asyncio.to_thread(export_kubernetes, backend, sandbox_id)
    else:
        reject("This runtime does not support rootfs fork.", "UNSUPPORTED_RUNTIME", 501)
    values.update({"timeout": request.timeout, "metadata": sandbox.metadata or {},
                   "entrypoint": list(DEFAULT_SNAPSHOT_RESTORE_ENTRYPOINT)})
    if sandbox.platform:
        values["platform"] = sandbox.platform.model_dump()
    overrides = request.overrides.model_dump(by_alias=True, exclude_unset=True)
    if has_egress and "networkPolicy" not in overrides:
        endpoint = await asyncio.to_thread(backend.get_endpoint, sandbox_id, 18080, True)
        async with httpx.AsyncClient(timeout=10, trust_env=False) as client:
            try:
                response = await client.get(f"http://{endpoint.endpoint.rstrip('/')}/policy", headers=endpoint.headers)
                response.raise_for_status()
                payload = response.json()
            except (httpx.HTTPError, ValueError):
                reject("Could not read the effective source network policy.", "POLICY_UNAVAILABLE")
            if not isinstance(payload, dict) or not isinstance(payload.get("policy"), dict):
                reject("Could not read the effective source network policy.", "POLICY_UNAVAILABLE")
            values["networkPolicy"] = payload["policy"]
    values.update(overrides)
    # Validate through the normal create schema before persisting any intent.
    validated = CreateSandboxRequest(snapshotId="fork-preflight", **values)
    backend._ensure_secure_access_support(validated)
    backend._ensure_network_policy_support(validated)
    if hasattr(backend, "_prepare_resource_limits"):
        backend._prepare_resource_limits(validated)
    return validated.model_dump(by_alias=True, exclude_none=True, exclude={"snapshot_id"})
