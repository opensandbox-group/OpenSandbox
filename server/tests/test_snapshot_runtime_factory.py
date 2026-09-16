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

from __future__ import annotations

from typing import cast

import pytest
from pydantic import SecretStr

from opensandbox_server.config import (
    AppConfig,
    KubernetesRuntimeConfig,
    PostgreSQLStoreConfig,
    RuntimeConfig,
    StoreConfig,
)
from opensandbox_server.services.docker.snapshot_runtime import DockerSnapshotRuntime
from opensandbox_server.services.fast_sandbox.snapshot_runtime import FastSandboxSnapshotRuntime
from opensandbox_server.services.k8s.snapshot_runtime import KubernetesSnapshotRuntime
from opensandbox_server.services.snapshot_runtime import SnapshotRuntime
from opensandbox_server.services.snapshot_runtime_factory import (
    CompositeSnapshotRuntime,
    create_snapshot_runtime,
)


def test_create_snapshot_runtime_selects_docker_runtime() -> None:
    config = AppConfig(runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:test"))
    docker_client = object()

    runtime = create_snapshot_runtime(config, docker_client=docker_client)

    assert isinstance(runtime, DockerSnapshotRuntime)


def test_create_snapshot_runtime_composes_kubernetes_and_fsb_runtimes() -> None:
    config = AppConfig(
        runtime=RuntimeConfig(type="kubernetes", execd_image="opensandbox/execd:test"),
        kubernetes=KubernetesRuntimeConfig(namespace="default"),
    )
    k8s_client = object()

    runtime = create_snapshot_runtime(config, k8s_client=k8s_client)

    assert isinstance(runtime, CompositeSnapshotRuntime)
    k8s_runtime = runtime.default
    assert isinstance(k8s_runtime, KubernetesSnapshotRuntime)
    assert isinstance(runtime.fsb, FastSandboxSnapshotRuntime)
    assert k8s_runtime._postgresql_ha_enabled is False


def test_postgresql_kubernetes_runtime_enables_ha_recovery() -> None:
    config = AppConfig(
        runtime=RuntimeConfig(type="kubernetes", execd_image="opensandbox/execd:test"),
        kubernetes=KubernetesRuntimeConfig(namespace="default"),
        store=StoreConfig(
            type="postgresql",
            postgresql=PostgreSQLStoreConfig(
                dsn=SecretStr("postgresql://postgres:postgres@localhost/opensandbox"),
            ),
        ),
    )

    runtime = create_snapshot_runtime(config, k8s_client=object())

    assert isinstance(runtime, CompositeSnapshotRuntime)
    k8s_runtime = runtime.default
    assert isinstance(k8s_runtime, KubernetesSnapshotRuntime)
    assert k8s_runtime._postgresql_ha_enabled is True


def test_create_snapshot_runtime_requires_docker_client_for_docker() -> None:
    config = AppConfig(runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:test"))

    with pytest.raises(ValueError, match="docker_client is required"):
        create_snapshot_runtime(config)


class _DispatchStubRuntime:
    def __init__(self, name: str) -> None:
        self.name = name
        self.preflight: list[str] = []
        self.created: list[tuple[str, str, str | None]] = []
        self.inspected: list[str] = []
        self.deleted: list[str] = []
        self.watch_started = False
        self.closed = False

    def supports_create_snapshot(self) -> bool:
        return True

    def create_snapshot_unsupported_message(self) -> str:
        return ""

    def preflight_create_snapshot(self, sandbox_id: str, *, namespace: str | None = None) -> None:
        self.preflight.append(sandbox_id)

    def create_snapshot(self, snapshot_id: str, sandbox_id: str, *, namespace: str | None = None):
        self.created.append((snapshot_id, sandbox_id, namespace))
        return None

    def get_snapshot_status(self, snapshot_id: str):
        return None

    def delete_snapshot(
        self,
        snapshot_id: str,
        image=None,
        *,
        namespace: str | None = None,
        source_sandbox_id: str | None = None,
    ) -> None:
        self.deleted.append(snapshot_id)

    def inspect_snapshot(
        self,
        snapshot_id: str,
        image=None,
        *,
        namespace: str | None = None,
        source_sandbox_id: str | None = None,
    ):
        self.inspected.append(snapshot_id)

    def start_status_watch(self, on_change, namespaces=()) -> None:
        self.watch_started = True

    def close(self) -> None:
        self.closed = True


def _composite(
    default: _DispatchStubRuntime,
    fsb: _DispatchStubRuntime | None = None,
) -> CompositeSnapshotRuntime:
    return CompositeSnapshotRuntime(
        cast(SnapshotRuntime, default),
        cast(SnapshotRuntime, fsb) if fsb is not None else None,
    )


def test_composite_runtime_dispatches_by_source_sandbox_prefix() -> None:
    default = _DispatchStubRuntime("k8s")
    fsb = _DispatchStubRuntime("fsb")
    composite = _composite(default, fsb)

    composite.preflight_create_snapshot("fsb-001", namespace="tenant-a")
    composite.preflight_create_snapshot("sbx-001")
    composite.create_snapshot("snap-1", "fsb-001", namespace="tenant-a")
    composite.create_snapshot("snap-2", "sbx-001")
    composite.inspect_snapshot("snap-1", source_sandbox_id="fsb-001")
    composite.inspect_snapshot("snap-2", source_sandbox_id="sbx-001")
    composite.delete_snapshot("snap-1", source_sandbox_id="fsb-001")
    composite.delete_snapshot("snap-2", source_sandbox_id="sbx-001")

    assert fsb.preflight == ["fsb-001"]
    assert default.preflight == ["sbx-001"]
    assert [created[1] for created in fsb.created] == ["fsb-001"]
    assert [created[1] for created in default.created] == ["sbx-001"]
    assert fsb.inspected == ["snap-1"]
    assert default.inspected == ["snap-2"]
    assert fsb.deleted == ["snap-1"]
    assert default.deleted == ["snap-2"]


def test_composite_runtime_fans_out_watch_and_close() -> None:
    default = _DispatchStubRuntime("k8s")
    fsb = _DispatchStubRuntime("fsb")
    composite = _composite(default, fsb)

    composite.start_status_watch(lambda snapshot_id, namespace: None)
    composite.close()

    assert default.watch_started and fsb.watch_started
    assert default.closed and fsb.closed


def test_composite_runtime_defaults_without_fsb_runtime() -> None:
    default = _DispatchStubRuntime("k8s")
    composite = _composite(default)

    composite.preflight_create_snapshot("fsb-001")
    composite.inspect_snapshot("snap-1", source_sandbox_id="fsb-001")

    assert default.preflight == ["fsb-001"]
    assert default.inspected == ["snap-1"]
