# Copyright 2026 Alibaba Group Holding Ltd.
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

from types import SimpleNamespace
from unittest.mock import MagicMock

import pytest
from kubernetes.client.exceptions import ApiException

from opensandbox_server.config import AppConfig, RuntimeConfig, SecureRuntimeConfig
from opensandbox_server.services.runtime_resolver import (
    SecureRuntimeResolver,
    validate_secure_runtime_on_startup,
)


def _config(runtime_type: str = "docker", secure_runtime=None):
    return AppConfig(
        runtime=RuntimeConfig(type=runtime_type, execd_image="opensandbox/execd:test"),
        secure_runtime=secure_runtime,
    )


def test_secure_runtime_resolver_disabled_without_runtime() -> None:
    resolver = SecureRuntimeResolver(_config())

    assert resolver.is_enabled() is False
    assert resolver.get_docker_runtime() is None
    assert resolver.get_k8s_runtime_class() is None


def test_secure_runtime_resolver_prefers_explicit_values_and_defaults() -> None:
    docker_resolver = SecureRuntimeResolver(
        _config(secure_runtime=SecureRuntimeConfig(type="gvisor", docker_runtime="custom-runsc"))
    )
    k8s_resolver = SecureRuntimeResolver(
        _config(
            runtime_type="kubernetes",
            secure_runtime=SecureRuntimeConfig(type="kata", k8s_runtime_class="kata-custom"),
        )
    )
    default_docker = SecureRuntimeResolver(
        _config(secure_runtime=SecureRuntimeConfig(type="gvisor", k8s_runtime_class="gvisor"))
    )
    default_k8s = SecureRuntimeResolver(
        _config(
            runtime_type="kubernetes",
            secure_runtime=SecureRuntimeConfig(type="kata", docker_runtime="kata-runtime"),
        )
    )

    assert docker_resolver.is_enabled() is True
    assert docker_resolver.get_docker_runtime() == "custom-runsc"
    assert k8s_resolver.get_k8s_runtime_class() == "kata-custom"
    assert default_docker.get_docker_runtime() == "runsc"
    assert default_k8s.get_k8s_runtime_class() == "kata-qemu"


@pytest.mark.asyncio
async def test_validate_secure_runtime_skips_when_disabled() -> None:
    docker_client = MagicMock()

    await validate_secure_runtime_on_startup(_config(), docker_client=docker_client)

    docker_client.info.assert_not_called()


@pytest.mark.asyncio
async def test_validate_secure_runtime_checks_docker_runtime() -> None:
    docker_client = MagicMock()
    docker_client.info.return_value = {"Runtimes": {"runsc": {"path": "/usr/bin/runsc"}}}
    config = _config(
        secure_runtime=SecureRuntimeConfig(type="gvisor", docker_runtime="runsc")
    )

    await validate_secure_runtime_on_startup(config, docker_client=docker_client)

    docker_client.info.assert_called_once()


@pytest.mark.asyncio
async def test_validate_secure_runtime_rejects_missing_docker_runtime() -> None:
    docker_client = MagicMock()
    docker_client.info.return_value = {"Runtimes": {"runc": {}}}
    config = _config(
        secure_runtime=SecureRuntimeConfig(type="gvisor", docker_runtime="runsc")
    )

    with pytest.raises(ValueError, match="runsc"):
        await validate_secure_runtime_on_startup(config, docker_client=docker_client)


@pytest.mark.asyncio
async def test_validate_secure_runtime_allows_missing_docker_client() -> None:
    config = _config(
        secure_runtime=SecureRuntimeConfig(type="gvisor", docker_runtime="runsc")
    )

    await validate_secure_runtime_on_startup(config, docker_client=None)


@pytest.mark.asyncio
async def test_validate_secure_runtime_checks_k8s_runtime_class() -> None:
    k8s_client = MagicMock()
    config = _config(
        runtime_type="kubernetes",
        secure_runtime=SecureRuntimeConfig(type="gvisor", k8s_runtime_class="gvisor"),
    )

    await validate_secure_runtime_on_startup(config, k8s_client=k8s_client)

    k8s_client.read_runtime_class.assert_called_once_with("gvisor")


@pytest.mark.asyncio
async def test_validate_secure_runtime_rejects_missing_k8s_runtime_class() -> None:
    k8s_client = MagicMock()
    k8s_client.read_runtime_class.side_effect = ApiException(status=404)
    config = _config(
        runtime_type="kubernetes",
        secure_runtime=SecureRuntimeConfig(type="gvisor", k8s_runtime_class="gvisor"),
    )

    with pytest.raises(ValueError, match="RuntimeClass 'gvisor'"):
        await validate_secure_runtime_on_startup(config, k8s_client=k8s_client)


@pytest.mark.asyncio
async def test_validate_secure_runtime_reraises_k8s_api_errors() -> None:
    k8s_client = MagicMock()
    k8s_client.read_runtime_class.side_effect = ApiException(status=500)
    config = _config(
        runtime_type="kubernetes",
        secure_runtime=SecureRuntimeConfig(type="gvisor", k8s_runtime_class="gvisor"),
    )

    with pytest.raises(ApiException):
        await validate_secure_runtime_on_startup(config, k8s_client=k8s_client)


@pytest.mark.asyncio
async def test_validate_secure_runtime_allows_missing_k8s_client() -> None:
    config = _config(
        runtime_type="kubernetes",
        secure_runtime=SecureRuntimeConfig(type="gvisor", k8s_runtime_class="gvisor"),
    )

    await validate_secure_runtime_on_startup(config, k8s_client=None)


@pytest.mark.asyncio
async def test_validate_secure_runtime_skips_unknown_runtime_type() -> None:
    config = SimpleNamespace(
        runtime=SimpleNamespace(type="custom"),
        secure_runtime=SimpleNamespace(
            type="gvisor",
            docker_runtime="runsc",
            k8s_runtime_class=None,
        ),
    )

    await validate_secure_runtime_on_startup(config)
