# Copyright 2025 The OpenSandbox Authors
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

from typing import Callable, Iterable, Optional

from opensandbox_server.config import AppConfig, KubernetesRuntimeConfig, get_config
from opensandbox_server.services.snapshot_runtime import (
    SnapshotRuntime,
    SnapshotRuntimeStatus,
)

_FSB_SOURCE_PREFIX = "fsb-"


class CompositeSnapshotRuntime:
    """Dispatch snapshot operations between coexisting backends.

    Sandbox creation (and therefore snapshot preflight/create) is selected by
    the source sandbox id prefix: ``fsb-`` sandboxes snapshot through the
    fast-sandbox runtime, everything else through the default runtime. Status
    and delete operations carry ``source_sandbox_id`` for the same dispatch.
    Watch and close fan out to every backend.
    """

    def __init__(self, default: SnapshotRuntime, fsb: Optional[SnapshotRuntime] = None) -> None:
        self._default = default
        self._fsb = fsb

    @property
    def default(self) -> SnapshotRuntime:
        return self._default

    @property
    def fsb(self) -> Optional[SnapshotRuntime]:
        return self._fsb

    def _for_source(self, source_sandbox_id: Optional[str]) -> SnapshotRuntime:
        if (
            self._fsb is not None
            and source_sandbox_id
            and source_sandbox_id.startswith(_FSB_SOURCE_PREFIX)
        ):
            return self._fsb
        return self._default

    def supports_create_snapshot(self) -> bool:
        return self._default.supports_create_snapshot() or (
            self._fsb is not None and self._fsb.supports_create_snapshot()
        )

    def create_snapshot_unsupported_message(self) -> str:
        return self._default.create_snapshot_unsupported_message()

    def preflight_create_snapshot(
        self,
        sandbox_id: str,
        *,
        namespace: str | None = None,
    ) -> None:
        self._for_source(sandbox_id).preflight_create_snapshot(sandbox_id, namespace=namespace)

    def create_snapshot(
        self,
        snapshot_id: str,
        sandbox_id: str,
        *,
        namespace: str | None = None,
    ) -> Optional[SnapshotRuntimeStatus]:
        return self._for_source(sandbox_id).create_snapshot(
            snapshot_id,
            sandbox_id,
            namespace=namespace,
        )

    def get_snapshot_status(self, snapshot_id: str) -> Optional[SnapshotRuntimeStatus]:
        return self._default.get_snapshot_status(snapshot_id)

    def delete_snapshot(
        self,
        snapshot_id: str,
        image: Optional[str] = None,
        *,
        namespace: str | None = None,
        source_sandbox_id: str | None = None,
    ) -> None:
        self._for_source(source_sandbox_id).delete_snapshot(
            snapshot_id,
            image,
            namespace=namespace,
        )

    def inspect_snapshot(
        self,
        snapshot_id: str,
        image: Optional[str] = None,
        *,
        namespace: str | None = None,
        source_sandbox_id: str | None = None,
    ) -> SnapshotRuntimeStatus:
        return self._for_source(source_sandbox_id).inspect_snapshot(
            snapshot_id,
            image,
            namespace=namespace,
        )

    def start_status_watch(
        self,
        on_change: Callable[[str, str], None],
        namespaces: Iterable[str] = (),
    ) -> None:
        for runtime in (self._default, self._fsb):
            start = getattr(runtime, "start_status_watch", None)
            if start is not None:
                start(on_change, namespaces)

    def close(self) -> None:
        for runtime in (self._default, self._fsb):
            close = getattr(runtime, "close", None)
            if close is not None:
                close()


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

        from opensandbox_server.services.fast_sandbox.fastpath_client import FastPathClient
        from opensandbox_server.services.fast_sandbox.snapshot_runtime import (
            FastSandboxSnapshotRuntime,
        )
        from opensandbox_server.services.k8s.client import K8sClient
        from opensandbox_server.services.k8s.snapshot_runtime import KubernetesSnapshotRuntime

        kubernetes_config = getattr(active_config, "kubernetes", None) or KubernetesRuntimeConfig()
        if k8s_client is None:
            k8s_client = K8sClient(kubernetes_config)

        namespace = kubernetes_config.namespace or "default"
        kubernetes_runtime: SnapshotRuntime = KubernetesSnapshotRuntime(
            k8s_client,
            namespace=namespace,
            postgresql_ha_enabled=active_config.store.type == "postgresql",
        )
        # fsb sandboxes (fsb-*) coexist with pod sandboxes under the same
        # Kubernetes-mode server; their snapshots go through FastPath. Both
        # runtimes share the client (informer keys differ by API group, and
        # stop_informers is idempotent).
        fastpath_client = FastPathClient(
            endpoint=kubernetes_config.fastpath_endpoint,
            timeout_seconds=kubernetes_config.fastpath_timeout_seconds,
        )
        fsb_runtime: SnapshotRuntime = FastSandboxSnapshotRuntime(
            fastpath_client,
            k8s_client,
            namespace=namespace,
        )
        return CompositeSnapshotRuntime(kubernetes_runtime, fsb_runtime)

    raise ValueError(f"Unsupported snapshot runtime type: {runtime_type}")


__all__ = [
    "CompositeSnapshotRuntime",
    "create_snapshot_runtime",
]
