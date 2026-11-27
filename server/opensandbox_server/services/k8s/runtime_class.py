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

"""
RuntimeClass of a BatchSandbox's running Pod.

The rootfs snapshot committer behind public snapshots and pause cannot capture
a gVisor sandbox's writes, so both operations read the handler from here
before they commit anything.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from opensandbox_server.services.k8s.workload_mapper import allocated_pod_names

BATCHSANDBOX_NAME_LABEL = "batch-sandbox.sandbox.opensandbox.io/name"


class RuntimeClassLookupError(Exception):
    """The runtime of a BatchSandbox's running Pod could not be verified."""


@dataclass(frozen=True)
class PodRuntime:
    runtime_class_name: str
    handler: str


def source_pod_runtime(
    k8s_client: Any,
    workload: Any,
    *,
    sandbox_id: str,
    namespace: str,
) -> PodRuntime | None:
    """Return the RuntimeClass and handler of the sandbox's running Pod.

    ``sandbox_id`` is the name of the BatchSandbox resource, which labels its
    Pods. Returns None when the Pod names no RuntimeClass, so it runs on the
    node's default runtime. Raises RuntimeClassLookupError when no running Pod
    is found or the RuntimeClass names no handler; errors of the Kubernetes
    client propagate to the caller.
    """
    pod = _source_pod(k8s_client, workload, sandbox_id=sandbox_id, namespace=namespace)
    runtime_class_name = _field(_field(pod, "spec"), "runtimeClassName", "runtime_class_name")
    if runtime_class_name is None:
        return None
    if not isinstance(runtime_class_name, str) or not runtime_class_name:
        raise RuntimeClassLookupError("source Pod has an invalid RuntimeClass name.")
    handler = _runtime_class_handler(k8s_client.read_runtime_class(runtime_class_name))
    if not handler:
        raise RuntimeClassLookupError(f"RuntimeClass {runtime_class_name!r} has no handler.")
    return PodRuntime(runtime_class_name=runtime_class_name, handler=handler)


def is_gvisor_handler(handler: str) -> bool:
    normalized = handler.lower()
    return normalized == "runsc" or normalized.startswith("runsc-")


def _source_pod(k8s_client: Any, workload: Any, *, sandbox_id: str, namespace: str) -> Any:
    for pod_name in allocated_pod_names(workload):
        pod = k8s_client.read_pod(namespace, pod_name)
        if _pod_is_running(pod):
            return pod

    pods = k8s_client.list_pods(
        namespace=namespace,
        label_selector=f"{BATCHSANDBOX_NAME_LABEL}={sandbox_id}",
    )
    for pod in pods:
        if _pod_is_running(pod):
            return pod

    fallback = k8s_client.read_pod(namespace, f"{sandbox_id}-0")
    if _pod_is_running(fallback):
        return fallback

    raise RuntimeClassLookupError("no running source Pod was found.")


def _pod_is_running(pod: Any) -> bool:
    return _field(_field(pod, "status"), "phase") == "Running"


def _runtime_class_handler(runtime_class: Any) -> str | None:
    handler = _field(runtime_class, "handler")
    return handler if isinstance(handler, str) and handler else None


def _field(value: Any, *names: str) -> Any:
    if isinstance(value, dict):
        for name in names:
            if name in value:
                return value[name]
        return None
    for name in names:
        field = getattr(value, name, None)
        if field is not None:
            return field
    return None
