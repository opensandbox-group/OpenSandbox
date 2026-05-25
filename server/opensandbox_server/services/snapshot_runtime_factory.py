# Copyright 2025 Alibaba Group Holding Ltd.
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
Factory for creating snapshot runtime implementations.
"""

from __future__ import annotations

from typing import Optional

from opensandbox_server.config import AppConfig, KubernetesRuntimeConfig, get_config
from opensandbox_server.services.snapshot_runtime import SnapshotRuntime


def create_snapshot_runtime(
    config: Optional[AppConfig] = None,
    *,
    docker_client=None,
    k8s_client=None,
) -> SnapshotRuntime:
    active_config = config or get_config()
    runtime_type = active_config.runtime.type.lower()

    if runtime_type == "docker":
        if docker_client is None:
            raise ValueError("docker_client is required when runtime.type = 'docker'.")
        from opensandbox_server.services.docker.snapshot_runtime import DockerSnapshotRuntime

        return DockerSnapshotRuntime(docker_client)

    if runtime_type == "kubernetes":
        from opensandbox_server.services.k8s.client import K8sClient
        from opensandbox_server.services.k8s.snapshot_runtime import KubernetesSnapshotRuntime

        kubernetes_config = getattr(active_config, "kubernetes", None) or KubernetesRuntimeConfig()
        if k8s_client is None:
            k8s_client = K8sClient(kubernetes_config)

        namespace = kubernetes_config.namespace or "default"
        return KubernetesSnapshotRuntime(
            k8s_client,
            namespace=namespace,
            wait_timeout_seconds=kubernetes_config.snapshot_create_timeout_seconds,
        )

    raise ValueError(f"Unsupported snapshot runtime type: {runtime_type}")


__all__ = [
    "create_snapshot_runtime",
]
