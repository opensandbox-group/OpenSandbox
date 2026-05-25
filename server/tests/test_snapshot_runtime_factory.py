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

import pytest

from opensandbox_server.config import AppConfig, KubernetesRuntimeConfig, RuntimeConfig
from opensandbox_server.services.docker.snapshot_runtime import DockerSnapshotRuntime
from opensandbox_server.services.k8s.snapshot_runtime import KubernetesSnapshotRuntime
from opensandbox_server.services.snapshot_runtime_factory import create_snapshot_runtime


def test_create_snapshot_runtime_selects_docker_runtime() -> None:
    config = AppConfig(runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:test"))
    docker_client = object()

    runtime = create_snapshot_runtime(config, docker_client=docker_client)

    assert isinstance(runtime, DockerSnapshotRuntime)


def test_create_snapshot_runtime_selects_kubernetes_runtime() -> None:
    config = AppConfig(
        runtime=RuntimeConfig(type="kubernetes", execd_image="opensandbox/execd:test"),
        kubernetes=KubernetesRuntimeConfig(namespace="default"),
    )
    k8s_client = object()

    runtime = create_snapshot_runtime(config, k8s_client=k8s_client)

    assert isinstance(runtime, KubernetesSnapshotRuntime)


def test_create_snapshot_runtime_uses_kubernetes_snapshot_create_timeout() -> None:
    config = AppConfig(
        runtime=RuntimeConfig(type="kubernetes", execd_image="opensandbox/execd:test"),
        kubernetes=KubernetesRuntimeConfig(
            namespace="default",
            snapshot_create_timeout_seconds=1234,
        ),
    )
    k8s_client = object()

    runtime = create_snapshot_runtime(config, k8s_client=k8s_client)

    assert isinstance(runtime, KubernetesSnapshotRuntime)
    assert runtime._wait_timeout_seconds == 1234


def test_create_snapshot_runtime_requires_docker_client_for_docker() -> None:
    config = AppConfig(runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:test"))

    with pytest.raises(ValueError, match="docker_client is required"):
        create_snapshot_runtime(config)
