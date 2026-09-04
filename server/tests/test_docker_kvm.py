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

import os
import stat
from types import SimpleNamespace
from unittest.mock import MagicMock, patch

import pytest
from fastapi import HTTPException, status

from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    ImageSpec,
    NetworkPolicy,
    PlatformSpec,
    ResourceLimits,
)
from opensandbox_server.config import AppConfig, EgressConfig, IngressConfig, RuntimeConfig
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.docker import DockerSandboxService


def _app_config() -> AppConfig:
    return AppConfig(
        runtime=RuntimeConfig(
            type="docker",
            execd_image="ghcr.io/opensandbox/platform:latest",
        ),
        ingress=IngressConfig(mode="direct"),
    )


def _request(
    kvm: str | None,
    *,
    network_policy: NetworkPolicy | None = None,
) -> CreateSandboxRequest:
    resources = {} if kvm is None else {"kvm": kvm}
    return CreateSandboxRequest(
        image=ImageSpec(uri="python:3.12", auth=None),
        snapshotId=None,
        platform=None,
        timeout=120,
        resourceLimits=ResourceLimits(root=resources),
        resourceRequests=None,
        env={},
        metadata={},
        entrypoint=["python"],
        lifecycle=None,
        networkPolicy=network_policy,
        credentialProxy=None,
        secureAccess=False,
        volumes=None,
        extensions=None,
    )


def _service(mock_docker, *, base_url: str = "http+docker://localhost"):
    client = MagicMock()
    client.api.base_url = base_url
    client.containers.list.return_value = []
    client.api.create_host_config.side_effect = lambda **kwargs: kwargs
    client.api.create_container.return_value = {"Id": "main-id"}
    client.containers.get.return_value = MagicMock(id="main-id")
    mock_docker.from_env.return_value = client
    return DockerSandboxService(config=_app_config()), client


def _kvm_stat(*, mode: int = stat.S_IFCHR | 0o660, device: int | None = None, gid: int = 123):
    return SimpleNamespace(
        st_mode=mode,
        st_rdev=os.makedev(10, 232) if device is None else device,
        st_gid=gid,
    )


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_linux_kvm_request_adds_minimal_device_and_group(mock_docker):
    service, client = _service(mock_docker)

    with (
        patch("os.lstat", return_value=_kvm_stat()),
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_prepare_sandbox_runtime"),
    ):
        await service.create_sandbox(_request("1"))

    host_config = client.api.create_host_config.call_args.kwargs
    assert host_config["devices"] == ["/dev/kvm:/dev/kvm:rw"]
    assert host_config["group_add"] == ["123"]
    assert "privileged" not in host_config
    assert "cap_add" not in host_config


@pytest.mark.parametrize("value", ["", "0", "01", "2", "true", "all", " 1"])
@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_kvm_request_is_exact_opt_in(mock_docker, value: str):
    service, client = _service(mock_docker)

    with (
        patch.object(service, "_ensure_image_available") as ensure_image_available,
        patch.object(service, "_prepare_sandbox_runtime") as prepare_sandbox_runtime,
        pytest.raises(HTTPException) as exc_info,
    ):
        await service.create_sandbox(_request(value))

    assert exc_info.value.status_code == status.HTTP_400_BAD_REQUEST
    assert SandboxErrorCodes.INVALID_PARAMETER in str(exc_info.value.detail)
    ensure_image_available.assert_not_called()
    prepare_sandbox_runtime.assert_not_called()
    client.images.get.assert_not_called()
    client.volumes.create.assert_not_called()
    client.api.create_container.assert_not_called()


@pytest.mark.parametrize(
    "device_result",
    [
        FileNotFoundError(),
        _kvm_stat(mode=stat.S_IFLNK | 0o777),
        _kvm_stat(mode=stat.S_IFREG | 0o660),
        _kvm_stat(device=os.makedev(10, 231)),
    ],
)
@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_kvm_request_rejects_unsafe_device_before_provisioning(
    mock_docker,
    device_result,
):
    service, client = _service(mock_docker)
    lstat = MagicMock(
        side_effect=device_result if isinstance(device_result, OSError) else None,
        return_value=None if isinstance(device_result, OSError) else device_result,
    )

    with (
        patch("os.lstat", lstat),
        patch.object(service, "_ensure_image_available") as ensure_image_available,
        patch.object(service, "_prepare_sandbox_runtime") as prepare_sandbox_runtime,
        pytest.raises(HTTPException) as exc_info,
    ):
        await service.create_sandbox(_request("1"))

    assert exc_info.value.status_code == status.HTTP_400_BAD_REQUEST
    assert SandboxErrorCodes.INVALID_PARAMETER in str(exc_info.value.detail)
    ensure_image_available.assert_not_called()
    prepare_sandbox_runtime.assert_not_called()
    client.images.get.assert_not_called()
    client.volumes.create.assert_not_called()
    client.api.create_container.assert_not_called()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "base_url",
    [
        "http://docker.example:2375",
        "http://localhost:2375",
        "https://docker.example:2376",
        "http+docker://ssh",
        "http+docker://localnpipe",
        "http+docker://localhost.example",
    ],
)
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_kvm_request_rejects_remote_docker(mock_docker, base_url: str):
    service, client = _service(mock_docker, base_url=base_url)

    with (
        patch("os.lstat") as lstat,
        patch.object(service, "_ensure_image_available") as ensure_image_available,
        patch.object(service, "_prepare_sandbox_runtime") as prepare_sandbox_runtime,
        pytest.raises(HTTPException) as exc_info,
    ):
        await service.create_sandbox(_request("1"))

    assert exc_info.value.status_code == status.HTTP_400_BAD_REQUEST
    assert SandboxErrorCodes.INVALID_PARAMETER in str(exc_info.value.detail)
    lstat.assert_not_called()
    ensure_image_available.assert_not_called()
    prepare_sandbox_runtime.assert_not_called()
    client.api.create_container.assert_not_called()


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_absent_kvm_resource_leaves_host_config_unchanged(mock_docker):
    service, client = _service(mock_docker)

    with (
        patch("os.lstat") as lstat,
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_prepare_sandbox_runtime"),
    ):
        await service.create_sandbox(_request(None))

    lstat.assert_not_called()
    host_config = client.api.create_host_config.call_args.kwargs
    assert "devices" not in host_config
    assert "group_add" not in host_config


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_linux_kvm_root_group_omits_supplementary_group(mock_docker):
    service, client = _service(mock_docker)

    with (
        patch("os.lstat", return_value=_kvm_stat(gid=0)),
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_prepare_sandbox_runtime"),
    ):
        await service.create_sandbox(_request("1"))

    host_config = client.api.create_host_config.call_args.kwargs
    assert host_config["devices"] == ["/dev/kvm:/dev/kvm:rw"]
    assert "group_add" not in host_config


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_windows_profile_does_not_use_generic_kvm_path(mock_docker):
    service, _ = _service(mock_docker)
    request = _request("true").model_copy(
        update={"platform": PlatformSpec(os="windows", arch="amd64")}
    )

    with (
        patch(
            "opensandbox_server.services.docker.docker_service.resolve_kvm_device_group"
        ) as resolve_kvm,
        patch.object(service, "_validate_volumes", return_value=({}, [])),
        patch.object(service, "_provision_sandbox", return_value=MagicMock()),
    ):
        await service.create_sandbox(request)

    resolve_kvm.assert_not_called()


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_kvm_is_attached_only_to_main_container_with_egress(mock_docker):
    service, client = _service(mock_docker)
    service.app_config.docker.network_mode = "bridge"
    service.network_mode = "bridge"
    service.app_config.egress = EgressConfig(image="egress:latest")
    client.api.create_container.side_effect = [{"Id": "sidecar-id"}, {"Id": "main-id"}]
    client.containers.get.side_effect = [MagicMock(id="sidecar-id"), MagicMock(id="main-id")]

    with (
        patch("os.lstat", return_value=_kvm_stat()),
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_prepare_sandbox_runtime"),
        patch.object(service, "_wait_for_egress_sidecar_ready"),
        patch(
            "opensandbox_server.services.docker.docker_service.allocate_port_bindings",
            return_value={
                "44772": ("0.0.0.0", 44772),
                "8080": ("0.0.0.0", 8080),
                "18080": ("0.0.0.0", 18080),
            },
        ),
    ):
        await service.create_sandbox(
            _request("1", network_policy=NetworkPolicy(defaultAction="deny", egress=[]))
        )

    sidecar_host_config = client.api.create_host_config.call_args_list[0].kwargs
    main_host_config = client.api.create_host_config.call_args_list[1].kwargs
    assert "devices" not in sidecar_host_config
    assert main_host_config["devices"] == ["/dev/kvm:/dev/kvm:rw"]
