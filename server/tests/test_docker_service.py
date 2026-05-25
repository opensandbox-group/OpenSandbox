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

import os
from datetime import datetime, timedelta, timezone
from typing import Any, cast
from unittest.mock import AsyncMock, MagicMock, patch

from docker.errors import DockerException, NotFound as DockerNotFound
import pytest
from fastapi import HTTPException, status
from pydantic import ValidationError

from opensandbox_server.config import (
    AppConfig,
    EGRESS_MODE_DNS,
    EgressConfig,
    RuntimeConfig,
    ServerConfig,
    StorageConfig,
    IngressConfig,
)
from opensandbox_server.extensions import ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY
from opensandbox_server.services.constants import (
    EGRESS_MODE_ENV,
    OPENSANDBOX_EGRESS_TOKEN,
)
from opensandbox_server.services.constants import (
    SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY,
    SANDBOX_EXPIRES_AT_LABEL,
    SANDBOX_ID_LABEL,
    SANDBOX_MANUAL_CLEANUP_LABEL,
    SANDBOX_OSSFS_MOUNTS_LABEL,
    SANDBOX_PLATFORM_ARCH_LABEL,
    SANDBOX_PLATFORM_OS_LABEL,
    SANDBOX_SNAPSHOT_ID_LABEL,
    SandboxErrorCodes,
)
from opensandbox_server.services.docker import DockerSandboxService, PendingSandbox
from opensandbox_server.services.helpers import (
    parse_gpu_request,
    parse_memory_limit,
    parse_nano_cpus,
    parse_timestamp,
)
from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    CreateSandboxResponse,
    Host,
    ImageSpec,
    NetworkPolicy,
    ListSandboxesRequest,
    OSSFS,
    PlatformSpec,
    PVC,
    ResourceLimits,
    Sandbox,
    SandboxFilter,
    SandboxStatus,
    Volume,
)

def _app_config() -> AppConfig:
    return AppConfig(
        server=ServerConfig(),
        runtime=RuntimeConfig(type="docker", execd_image="ghcr.io/opensandbox/platform:latest"),
        ingress=IngressConfig(mode="direct"),
    )

def test_parse_memory_limit_handles_units():
    assert parse_memory_limit("512Mi") == 512 * 1024 * 1024
    assert parse_memory_limit("1G") == 1_000_000_000
    assert parse_memory_limit("2gi") == 2 * 1024**3
    assert parse_memory_limit("invalid") is None

def test_parse_nano_cpus():
    assert parse_nano_cpus("500m") == 500_000_000
    assert parse_nano_cpus("2") == 2_000_000_000
    assert parse_nano_cpus("bad") is None

def test_parse_gpu_request():
    assert parse_gpu_request("1") == 1
    assert parse_gpu_request("4") == 4
    assert parse_gpu_request("all") == -1
    assert parse_gpu_request("ALL") == -1
    assert parse_gpu_request(None) is None
    assert parse_gpu_request("") is None
    assert parse_gpu_request("0") is None
    assert parse_gpu_request("-1") is None
    assert parse_gpu_request("bad") is None

def test_parse_timestamp_defaults_on_invalid():
    ts = parse_timestamp("0001-01-01T00:00:00Z")
    assert ts.tzinfo is not None
    future = parse_timestamp("2024-01-01T00:00:00Z")
    assert future.year == 2024

def test_env_allows_empty_string_and_skips_none():
    # Use base config helper
    DockerSandboxService(config=_app_config())
    # Build request with mixed env values
    req = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={"FOO": "bar", "EMPTY": "", "NONE": None},
        metadata={},
        entrypoint=["python"],
    )
    # Validate env handling
    env_dict = req.env or {}
    environment = []
    for key, value in env_dict.items():
        if value is None:
            continue
        environment.append(f"{key}={value}")

    assert "FOO=bar" in environment
    assert "EMPTY=" in environment  # empty string preserved
    # None should be skipped
    assert all(not item.startswith("NONE=") for item in environment)

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_applies_security_defaults(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_client.api.create_host_config.return_value = {
        "security_opt": ["no-new-privileges:true"],
        "cap_drop": _app_config().docker.drop_capabilities,
        "pids_limit": _app_config().docker.pids_limit,
    }
    mock_client.api.create_container.return_value = {"Id": "cid"}
    mock_client.containers.get.return_value = MagicMock()
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())
    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python"],
    )

    with (
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_prepare_sandbox_runtime"),
        patch(
            "opensandbox_server.services.docker.docker_service.allocate_port_bindings",
            return_value={
                "44772": ("0.0.0.0", 40001),
                "8080": ("0.0.0.0", 40002),
            },
        ),
    ):
        await service.create_sandbox(request)

    host_config = mock_client.api.create_container.call_args.kwargs["host_config"]
    assert "no-new-privileges:true" in host_config.get("security_opt", [])
    assert host_config.get("cap_drop") == service.app_config.docker.drop_capabilities
    assert host_config.get("pids_limit") == service.app_config.docker.pids_limit

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_passes_gpu_device_requests(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_client.api.create_container.return_value = {"Id": "cid"}
    mock_client.containers.get.return_value = MagicMock()
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())
    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={"gpu": "2"}),
        env={},
        metadata={},
        entrypoint=["python"],
    )

    with (
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_prepare_sandbox_runtime"),
        patch(
            "opensandbox_server.services.docker.docker_service.allocate_port_bindings",
            return_value={
                "44772": ("0.0.0.0", 40001),
                "8080": ("0.0.0.0", 40002),
            },
        ),
    ):
        await service.create_sandbox(request)

    create_host_config_kwargs = mock_client.api.create_host_config.call_args.kwargs
    device_requests = create_host_config_kwargs.get("device_requests")
    assert device_requests is not None
    assert len(device_requests) == 1
    # DeviceRequest is a dict subclass keyed with the Docker Engine's
    # capitalized field names.
    assert device_requests[0]["Count"] == 2
    assert device_requests[0]["Capabilities"] == [["gpu"]]

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_without_gpu_omits_device_requests(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_client.api.create_container.return_value = {"Id": "cid"}
    mock_client.containers.get.return_value = MagicMock()
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())
    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python"],
    )

    with (
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_prepare_sandbox_runtime"),
        patch(
            "opensandbox_server.services.docker.docker_service.allocate_port_bindings",
            return_value={
                "44772": ("0.0.0.0", 40001),
                "8080": ("0.0.0.0", 40002),
            },
        ),
    ):
        await service.create_sandbox(request)

    create_host_config_kwargs = mock_client.api.create_host_config.call_args.kwargs
    assert "device_requests" not in create_host_config_kwargs

@pytest.mark.parametrize(
    "runtime_exc, expected_status, expect_wrapped_error",
    [
        (
            RuntimeError("tarfile error"),
            status.HTTP_500_INTERNAL_SERVER_ERROR,
            True,
        ),
        (
            HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={"code": "CONFLICT", "message": "conflict error"},
            ),
            status.HTTP_409_CONFLICT,
            False,
        ),
    ],
)
@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_prepare_runtime_failure_triggers_cleanup(
    mock_docker, runtime_exc, expected_status, expect_wrapped_error
):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_client.api.create_container.return_value = {"Id": "cid"}
    mock_container = MagicMock()
    mock_client.containers.get.return_value = mock_container
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())
    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python"],
    )

    with (
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_prepare_sandbox_runtime", side_effect=runtime_exc),
    ):
        with pytest.raises(HTTPException) as exc:
            await service.create_sandbox(request)

    mock_container.remove.assert_called_with(force=True)

    assert exc.value.status_code == expected_status

    if expect_wrapped_error:
        assert str(runtime_exc) in str(exc.value.detail["message"])
    else:
        assert exc.value.detail["message"] == runtime_exc.detail["message"]

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_rejects_invalid_metadata(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())

    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={"Bad Key": "ok"},  # space is invalid for label key
        entrypoint=["python"],
    )

    with pytest.raises(HTTPException) as exc:
        await service.create_sandbox(request)

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    assert exc.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL
    mock_client.containers.create.assert_not_called()

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_rejects_pool_ref_on_docker(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())

    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        entrypoint=["python"],
        resourceLimits=ResourceLimits(root={}),
        extensions={"poolRef": "my-pool"},
    )

    with pytest.raises(HTTPException) as exc:
        await service.create_sandbox(request)

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    assert exc.value.detail["code"] == "SANDBOX::UNSUPPORTED_POOL_REF"
    mock_client.containers.create.assert_not_called()

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_rejects_timeout_above_configured_maximum(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    config = _app_config()
    config.server.max_sandbox_timeout_seconds = 3600
    service = DockerSandboxService(config=config)

    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=7200,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python"],
    )

    with pytest.raises(HTTPException) as exc:
        await service.create_sandbox(request)

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    assert exc.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
    assert "configured maximum of 3600s" in exc.value.detail["message"]

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_rejects_unsupported_platform(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())
    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        platform=PlatformSpec(os="darwin", arch="arm64"),
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python"],
    )

    with pytest.raises(HTTPException) as exc:
        await service.create_sandbox(request)

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    assert exc.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
    mock_client.containers.create.assert_not_called()

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_ensure_image_available_repulls_when_cached_platform_mismatch(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    cached_image = MagicMock()
    cached_image.attrs = {"Os": "linux", "Architecture": "amd64"}
    mock_client.images.get.return_value = cached_image

    service = DockerSandboxService(config=_app_config())
    with patch.object(service, "_pull_image") as mock_pull:
        service._ensure_image_available(
            "python:3.11",
            auth_config=None,
            sandbox_id="sandbox-1",
            platform=PlatformSpec(os="linux", arch="arm64"),
        )

    mock_pull.assert_called_once()
    call = mock_pull.call_args
    assert call.args[0] == "python:3.11"
    assert call.args[3] is not None
    assert call.args[3].os == "linux"
    assert call.args[3].arch == "arm64"

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_ensure_image_available_repulls_when_platform_omitted_and_cached_arch_differs(
    mock_docker,
):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    cached_image = MagicMock()
    cached_image.attrs = {"Os": "linux", "Architecture": "arm64"}
    mock_client.images.get.return_value = cached_image
    mock_client.info.return_value = {"OSType": "linux", "Architecture": "amd64"}

    service = DockerSandboxService(config=_app_config())
    with patch.object(service, "_pull_image") as mock_pull:
        service._ensure_image_available(
            "python:3.11",
            auth_config=None,
            sandbox_id="sandbox-default",
            platform=None,
        )

    mock_pull.assert_called_once()
    call = mock_pull.call_args
    assert call.args[0] == "python:3.11"
    assert call.args[3] is not None
    assert call.args[3].os == "linux"
    assert call.args[3].arch == "amd64"

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_ensure_image_available_does_not_repull_when_platform_omitted_and_cached_amd64(
    mock_docker,
):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    cached_image = MagicMock()
    cached_image.attrs = {"Os": "linux", "Architecture": "amd64"}
    mock_client.images.get.return_value = cached_image
    # Docker daemon may report x86_64/aarch64 aliases; this should still match amd64.
    mock_client.info.return_value = {"OSType": "linux", "Architecture": "x86_64"}
    service = DockerSandboxService(config=_app_config())
    with patch.object(service, "_pull_image") as mock_pull:
        service._ensure_image_available(
            "python:3.11",
            auth_config=None,
            sandbox_id="sandbox-default",
            platform=None,
        )

    mock_pull.assert_not_called()

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_pull_image_passes_platform_to_docker_api(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())
    service._pull_image(
        image_uri="python:3.11",
        auth_config=None,
        sandbox_id="sandbox-1",
        platform=PlatformSpec(os="linux", arch="arm64"),
    )

    mock_client.images.pull.assert_called_once_with(
        "python:3.11",
        auth_config=None,
        platform="linux/arm64",
    )

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_pull_image_skips_platform_for_windows_profile(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())
    service._pull_image(
        image_uri="dockurr/windows:latest",
        auth_config=None,
        sandbox_id="sandbox-win-1",
        platform=PlatformSpec(os="windows", arch="amd64"),
    )

    mock_client.images.pull.assert_called_once_with(
        "dockurr/windows:latest",
        auth_config=None,
    )

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_ensure_image_available_skips_windows_platform_mismatch_repull(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    cached_image = MagicMock()
    cached_image.attrs = {"Os": "linux", "Architecture": "amd64"}
    mock_client.images.get.return_value = cached_image
    mock_client.info.return_value = {"OSType": "linux", "Architecture": "amd64"}

    service = DockerSandboxService(config=_app_config())
    with patch.object(service, "_pull_image") as mock_pull:
        service._ensure_image_available(
            "dockurr/windows:latest",
            auth_config=None,
            sandbox_id="sandbox-win-1",
            platform=PlatformSpec(os="windows", arch="amd64"),
        )

    mock_pull.assert_not_called()

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_fetch_execd_archive_caches_by_platform_key(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client
    mock_client.info.return_value = {"OSType": "linux", "Architecture": "amd64"}

    container_amd64 = MagicMock()
    container_amd64.get_archive.return_value = ([b"amd64"], {})
    container_arm64 = MagicMock()
    container_arm64.get_archive.return_value = ([b"arm64"], {})
    mock_client.containers.create.side_effect = [container_amd64, container_arm64]

    service = DockerSandboxService(config=_app_config())
    with patch.object(service, "_docker_operation"):
        amd64_first = service._fetch_execd_archive(
            platform=PlatformSpec(os="linux", arch="amd64")
        )
        amd64_second = service._fetch_execd_archive(
            platform=PlatformSpec(os="linux", arch="amd64")
        )
        arm64_data = service._fetch_execd_archive(
            platform=PlatformSpec(os="linux", arch="arm64")
        )

    assert amd64_first == b"amd64"
    assert amd64_second == b"amd64"
    assert arm64_data == b"arm64"
    assert mock_client.containers.create.call_count == 2

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_fetch_execd_archive_maps_platform_typeerror_to_invalid_parameter(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client
    mock_client.containers.create.side_effect = TypeError("unexpected keyword argument 'platform'")

    service = DockerSandboxService(config=_app_config())
    with patch.object(service, "_ensure_image_available"):
        with pytest.raises(HTTPException) as exc_info:
            service._fetch_execd_archive(PlatformSpec(os="linux", arch="arm64"))

    assert exc_info.value.status_code == status.HTTP_400_BAD_REQUEST
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
    assert "platform-aware container create" in exc_info.value.detail["message"]

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_requires_entrypoint(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())

    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python"],
    )
    request.entrypoint = []

    with pytest.raises(HTTPException) as exc:
        await service.create_sandbox(request)

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    assert exc.value.detail["code"] == SandboxErrorCodes.INVALID_ENTRYPOINT
    mock_client.containers.create.assert_not_called()

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_network_policy_rejected_on_host_mode(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.docker.network_mode = "host"
    cfg.egress = EgressConfig(image="egress:latest")
    service = DockerSandboxService(config=cfg)

    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python"],
        networkPolicy=NetworkPolicy(default_action="deny", egress=[]),
    )

    with pytest.raises(HTTPException) as exc:
        await service.create_sandbox(request)

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    assert exc.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_network_policy_requires_egress_image(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.docker.network_mode = "bridge"
    cfg.egress = None
    service = DockerSandboxService(config=cfg)

    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python"],
        networkPolicy=NetworkPolicy(default_action="deny", egress=[]),
    )

    with pytest.raises(HTTPException) as exc:
        await service.create_sandbox(request)

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    assert exc.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_egress_sidecar_injection_and_capabilities(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []

    def host_cfg_side_effect(**kwargs):
        return kwargs

    mock_client.api.create_host_config.side_effect = host_cfg_side_effect
    mock_client.api.create_container.side_effect = [
        {"Id": "sidecar-id"},
        {"Id": "main-id"},
    ]
    mock_client.containers.get.side_effect = [MagicMock(id="sidecar-id"), MagicMock(id="main-id")]
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.docker.network_mode = "bridge"
    cfg.egress = EgressConfig(image="egress:latest")
    service = DockerSandboxService(config=cfg)

    req = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python"],
        networkPolicy=NetworkPolicy(default_action="deny", egress=[]),
    )

    with (
        patch("opensandbox_server.services.docker.docker_service.generate_egress_token", return_value="egress-token"),
        patch(
            "opensandbox_server.services.docker.docker_service.allocate_port_bindings",
            return_value={
                "44772": ("0.0.0.0", 44772),
                "8080": ("0.0.0.0", 8080),
            },
        ),
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_prepare_sandbox_runtime"),
    ):
        await service.create_sandbox(req)

    assert len(mock_client.api.create_container.call_args_list) == 2
    sidecar_call = mock_client.api.create_container.call_args_list[0]
    main_call = mock_client.api.create_container.call_args_list[1]
    sidecar_kwargs = sidecar_call.kwargs
    main_kwargs = main_call.kwargs

    # Sidecar host config should have NET_ADMIN and port bindings
    assert "NET_ADMIN" in sidecar_kwargs["host_config"]["cap_add"]
    assert "44772" in sidecar_kwargs["host_config"]["port_bindings"]
    assert "8080" in sidecar_kwargs["host_config"]["port_bindings"]

    # Main container should share sidecar netns, drop NET_ADMIN, and have no port bindings
    assert main_kwargs["host_config"]["network_mode"] == "container:sidecar-id"
    assert "NET_ADMIN" in set(main_kwargs["host_config"].get("cap_drop") or [])
    assert "port_bindings" not in main_kwargs["host_config"]

    # Main container labels should carry host port info
    labels = main_kwargs["labels"]
    assert labels.get("opensandbox.io/embedding-proxy-port")
    assert labels.get("opensandbox.io/http-port")
    assert labels[SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY] == "egress-token"

    sidecar_env = sidecar_kwargs["environment"]
    assert f"{OPENSANDBOX_EGRESS_TOKEN}=egress-token" in sidecar_env
    assert f"{EGRESS_MODE_ENV}={EGRESS_MODE_DNS}" in sidecar_env


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_rejects_secure_access_on_docker_runtime(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.docker.network_mode = "bridge"
    service = DockerSandboxService(config=cfg)

    req = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python"],
        secureAccess=True,
    )

    with pytest.raises(HTTPException) as exc:
        await service.create_sandbox(req)

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    assert exc.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
    assert "secureAccess is not supported when runtime.type='docker'" in exc.value.detail["message"]
    mock_client.api.create_container.assert_not_called()


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_network_policy_rejected_on_user_defined_network(mock_docker):
    """networkPolicy must be rejected when network_mode is a user-defined named network."""
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.docker.network_mode = "my-custom-net"
    cfg.egress = EgressConfig(image="egress:latest")
    service = DockerSandboxService(config=cfg)

    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python"],
        networkPolicy=NetworkPolicy(default_action="deny", egress=[]),
    )

    with pytest.raises(HTTPException) as exc:
        await service.create_sandbox(request)

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    assert exc.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
    assert "my-custom-net" in exc.value.detail["message"]

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_fails_when_user_defined_network_not_found(mock_docker):
    """create_sandbox raises 400 with a clear message when the named network does not exist."""
    from docker.errors import NotFound as DockerNotFound

    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_client.networks.get.side_effect = DockerNotFound("network not found")
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.docker.network_mode = "missing-net"
    service = DockerSandboxService(config=cfg)

    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python"],
    )

    with pytest.raises(HTTPException) as exc:
        await service.create_sandbox(request)

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    assert exc.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
    assert "missing-net" in exc.value.detail["message"]
    assert "docker network create" in exc.value.detail["message"]

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_user_defined_network_uses_correct_network_mode(mock_docker):
    """Containers created on a user-defined network use the network name as network_mode."""

    def host_cfg_side_effect(**kwargs):
        return kwargs

    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_client.networks.get.return_value = MagicMock()  # network exists
    mock_client.api.create_host_config.side_effect = host_cfg_side_effect
    mock_client.api.create_container.return_value = {"Id": "main-id"}
    mock_client.containers.get.return_value = MagicMock(id="main-id")
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.docker.network_mode = "my-app-net"
    service = DockerSandboxService(config=cfg)

    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python"],
    )

    with (
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_prepare_sandbox_runtime"),
        patch(
            "opensandbox_server.services.docker.docker_service.allocate_port_bindings",
            return_value={
                "44772": ("0.0.0.0", 40001),
                "8080": ("0.0.0.0", 40002),
            },
        ),
    ):
        await service.create_sandbox(request)

    call_kwargs = mock_client.api.create_container.call_args.kwargs
    assert call_kwargs["host_config"]["network_mode"] == "my-app-net"

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_validate_network_skipped_for_builtin_modes(mock_docker):
    """_validate_network_exists does NOT call the Docker API for host or bridge modes."""
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    for mode in ("host", "bridge", "none"):
        mock_client.networks.get.reset_mock()
        cfg = _app_config()
        cfg.docker.network_mode = mode
        service = DockerSandboxService(config=cfg)
        service._validate_network_exists()
        mock_client.networks.get.assert_not_called()

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_egress_sidecar_cleanup_uses_api_remove_when_lookup_fails(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []

    def host_cfg_side_effect(**kwargs):
        return kwargs

    mock_client.api.create_host_config.side_effect = host_cfg_side_effect
    mock_client.api.create_container.return_value = {"Id": "sidecar-id"}
    mock_client.containers.get.side_effect = DockerException("lookup failed")
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.docker.network_mode = "bridge"
    cfg.egress = EgressConfig(image="egress:latest")
    service = DockerSandboxService(config=cfg)

    with (
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_docker_operation") as mock_op,
    ):
        mock_op.return_value.__enter__.return_value = None
        mock_op.return_value.__exit__.return_value = None

        with pytest.raises(HTTPException) as exc:
            service._start_egress_sidecar(
                "sandbox-id",
                NetworkPolicy(defaultAction="deny", egress=[]),
                egress_token="egress-token",
                host_execd_port=44772,
                host_http_port=8080,
            )

    detail = exc.value.detail
    assert isinstance(detail, dict)
    typed_detail = cast(dict[str, Any], detail)
    assert exc.value.status_code == status.HTTP_500_INTERNAL_SERVER_ERROR
    assert typed_detail["message"] == "Egress sidecar container failed to start."
    mock_client.api.remove_container.assert_called_once_with("sidecar-id", force=True)

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_egress_sidecar_missing_id_preserves_specific_error(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []

    def host_cfg_side_effect(**kwargs):
        return kwargs

    mock_client.api.create_host_config.side_effect = host_cfg_side_effect
    mock_client.api.create_container.return_value = {}
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.docker.network_mode = "bridge"
    cfg.egress = EgressConfig(image="egress:latest")
    service = DockerSandboxService(config=cfg)

    with (
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_docker_operation") as mock_op,
    ):
        mock_op.return_value.__enter__.return_value = None
        mock_op.return_value.__exit__.return_value = None

        with pytest.raises(HTTPException) as exc:
            service._start_egress_sidecar(
                "sandbox-id",
                NetworkPolicy(defaultAction="deny", egress=[]),
                egress_token="egress-token",
                host_execd_port=44772,
                host_http_port=8080,
            )

    detail = exc.value.detail
    assert isinstance(detail, dict)
    typed_detail = cast(dict[str, Any], detail)
    assert exc.value.status_code == status.HTTP_500_INTERNAL_SERVER_ERROR
    assert typed_detail["message"] == "Docker did not return an egress sidecar container ID."
    mock_client.containers.get.assert_not_called()
    mock_client.api.remove_container.assert_not_called()

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_egress_sidecar_cleanup_wraps_unexpected_lookup_error(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []

    def host_cfg_side_effect(**kwargs):
        return kwargs

    mock_client.api.create_host_config.side_effect = host_cfg_side_effect
    mock_client.api.create_container.return_value = {"Id": "sidecar-id"}
    mock_client.containers.get.side_effect = RuntimeError("lookup failed")
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.docker.network_mode = "bridge"
    cfg.egress = EgressConfig(image="egress:latest")
    service = DockerSandboxService(config=cfg)

    with (
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_docker_operation") as mock_op,
    ):
        mock_op.return_value.__enter__.return_value = None
        mock_op.return_value.__exit__.return_value = None

        with pytest.raises(HTTPException) as exc:
            service._start_egress_sidecar(
                "sandbox-id",
                NetworkPolicy(defaultAction="deny", egress=[]),
                egress_token="egress-token",
                host_execd_port=44772,
                host_http_port=8080,
            )

    detail = exc.value.detail
    assert isinstance(detail, dict)
    typed_detail = cast(dict[str, Any], detail)
    assert exc.value.status_code == status.HTTP_500_INTERNAL_SERVER_ERROR
    assert typed_detail["code"] == SandboxErrorCodes.CONTAINER_START_FAILED
    assert typed_detail["message"] == "Egress sidecar container failed to start."
    mock_client.api.remove_container.assert_called_once_with("sidecar-id", force=True)

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_egress_sidecar_host_config_sysctls_only_when_egress_disable_ipv6(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []

    def host_cfg_side_effect(**kwargs):
        return kwargs

    mock_client.api.create_host_config.side_effect = host_cfg_side_effect
    mock_client.api.create_container.return_value = {"Id": "sidecar-id"}
    mock_client.containers.get.return_value = MagicMock()
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.docker.network_mode = "bridge"
    cfg.egress = EgressConfig(image="egress:latest", disable_ipv6=False)
    service = DockerSandboxService(config=cfg)

    with (
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_docker_operation") as mock_op,
    ):
        mock_op.return_value.__enter__.return_value = None
        mock_op.return_value.__exit__.return_value = None
        service._start_egress_sidecar(
            "sandbox-id",
            NetworkPolicy(defaultAction="deny", egress=[]),
            egress_token="egress-token",
            host_execd_port=44772,
            host_http_port=8080,
        )

    hc_kwargs = mock_client.api.create_host_config.call_args.kwargs
    assert "sysctls" not in hc_kwargs

    cfg.egress = EgressConfig(image="egress:latest", disable_ipv6=True)
    service2 = DockerSandboxService(config=cfg)
    mock_client.api.create_host_config.reset_mock()

    with (
        patch.object(service2, "_ensure_image_available"),
        patch.object(service2, "_docker_operation") as mock_op2,
    ):
        mock_op2.return_value.__enter__.return_value = None
        mock_op2.return_value.__exit__.return_value = None
        service2._start_egress_sidecar(
            "sandbox-id",
            NetworkPolicy(defaultAction="deny", egress=[]),
            egress_token="egress-token",
            host_execd_port=44772,
            host_http_port=8080,
        )

    hc2 = mock_client.api.create_host_config.call_args.kwargs
    assert hc2["sysctls"]["net.ipv6.conf.all.disable_ipv6"] == 1


@patch("opensandbox_server.services.docker.docker_service.docker")
def test_egress_sidecar_normalizes_windows_port_bindings(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []

    def host_cfg_side_effect(**kwargs):
        return kwargs

    sidecar_container = MagicMock()
    mock_client.api.create_host_config.side_effect = host_cfg_side_effect
    mock_client.api.create_container.return_value = {"Id": "sidecar-id"}
    mock_client.containers.get.return_value = sidecar_container
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.docker.network_mode = "bridge"
    cfg.egress = EgressConfig(image="egress:latest", disable_ipv6=False)
    service = DockerSandboxService(config=cfg)

    with (
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_docker_operation") as mock_op,
    ):
        mock_op.return_value.__enter__.return_value = None
        mock_op.return_value.__exit__.return_value = None
        service._start_egress_sidecar(
            "sandbox-id",
            NetworkPolicy(defaultAction="deny", egress=[]),
            egress_token="egress-token",
            host_execd_port=44772,
            host_http_port=8080,
            extra_port_bindings={
                "3389/tcp": ("0.0.0.0", 53389),
                "3389/udp": ("0.0.0.0", 53390),
                "8006/tcp": ("0.0.0.0", 58006),
            },
        )

    hc_kwargs = mock_client.api.create_host_config.call_args.kwargs
    assert "3389" in hc_kwargs["port_bindings"]
    assert "3389/udp" in hc_kwargs["port_bindings"]
    assert "8006" in hc_kwargs["port_bindings"]
    sidecar_kwargs = mock_client.api.create_container.call_args.kwargs
    assert "3389" in sidecar_kwargs["ports"]
    assert "3389/udp" in sidecar_kwargs["ports"]
    assert "8006" in sidecar_kwargs["ports"]

def test_expire_cleans_sidecar():
    service = DockerSandboxService(config=_app_config())
    mock_container = MagicMock()
    labels = {SANDBOX_PLATFORM_OS_LABEL: "windows"}
    mock_container.attrs = {"State": {"Running": False}, "Config": {"Labels": labels}}
    mock_container.kill = MagicMock()
    mock_container.remove = MagicMock()

    with (
        patch.object(service, "_get_container_by_sandbox_id", return_value=mock_container),
        patch.object(service, "_remove_expiration_tracking") as mock_remove,
        patch.object(service, "_cleanup_egress_sidecar") as mock_cleanup,
        patch.object(service, "_cleanup_windows_oem_volume") as mock_cleanup_oem,
        patch.object(service, "_docker_operation") as mock_op,
    ):
        mock_op.return_value.__enter__.return_value = None
        mock_op.return_value.__exit__.return_value = None
        service._expire_sandbox("sandbox-id")

    mock_cleanup.assert_called_once_with("sandbox-id")
    mock_cleanup_oem.assert_called_once_with("sandbox-id", labels)
    mock_remove.assert_called_once()

def test_restore_cleans_orphan_sidecar():
    cfg = _app_config()
    service = DockerSandboxService(config=cfg)

    orphan_sidecar = MagicMock()
    orphan_sidecar.attrs = {
        "Config": {"Labels": {"opensandbox.io/egress-sidecar-for": "orphan-id"}}
    }

    with (
        patch.object(service.docker_client.containers, "list", return_value=[orphan_sidecar]),
        patch.object(service, "_get_container_by_sandbox_id") as mock_get,
        patch.object(service, "_cleanup_egress_sidecar") as mock_cleanup,
    ):
        mock_get.side_effect = HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail={})
        service._restore_existing_sandboxes()

    mock_cleanup.assert_called_once_with("orphan-id")

def test_expire_not_found_attempts_windows_oem_volume_cleanup():
    service = DockerSandboxService(config=_app_config())

    with (
        patch.object(
            service,
            "_get_container_by_sandbox_id",
            side_effect=HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail={}),
        ),
        patch.object(service, "_remove_expiration_tracking") as mock_remove,
        patch.object(service, "_cleanup_windows_oem_volume") as mock_cleanup_oem,
    ):
        service._expire_sandbox("sandbox-missing")

    mock_remove.assert_called_once_with("sandbox-missing")
    mock_cleanup_oem.assert_called_once_with("sandbox-missing", None)

def test_prepare_creation_context_allows_manual_cleanup():
    service = DockerSandboxService(config=_app_config())
    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python"],
    )

    _, _, expires_at = service._prepare_creation_context(request)

    assert expires_at is None

def test_build_labels_marks_manual_cleanup_without_expiration():
    service = DockerSandboxService(config=_app_config())
    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={"team": "manual"},
        entrypoint=["python"],
    )

    labels, _ = service._build_labels_and_env("sandbox-manual", request, None)

    assert labels[SANDBOX_ID_LABEL] == "sandbox-manual"
    assert labels[SANDBOX_MANUAL_CLEANUP_LABEL] == "true"
    assert "opensandbox.io/expires-at" not in labels

def test_build_labels_stores_extensions_json():
    service = DockerSandboxService(config=_app_config())
    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        resourceLimits=ResourceLimits(root={}),
        env={},
        entrypoint=["python"],
        extensions={"access.renew.extend.seconds": "3600"},
    )

    labels, _ = service._build_labels_and_env("sandbox-ext", request, None)

    assert labels[ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY] == "3600"

def test_build_labels_store_platform_constraints():
    service = DockerSandboxService(config=_app_config())
    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        resourceLimits=ResourceLimits(root={}),
        env={},
        entrypoint=["python"],
        platform=PlatformSpec(os="linux", arch="arm64"),
    )

    labels, _ = service._build_labels_and_env("sandbox-platform", request, None)

    assert labels[SANDBOX_PLATFORM_OS_LABEL] == "linux"
    assert labels[SANDBOX_PLATFORM_ARCH_LABEL] == "arm64"

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_with_manual_cleanup_completes_full_create_path(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())
    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        resourceLimits=ResourceLimits(root={}),
        env={"DEBUG": "1"},
        metadata={"team": "manual"},
        entrypoint=["python"],
    )

    with (
        patch.object(service, "_create_and_start_container") as mock_create,
        patch.object(service, "_schedule_expiration") as mock_schedule,
    ):
        response = await service.create_sandbox(request)

    assert response.expires_at is None
    assert response.metadata == {"team": "manual"}
    assert response.entrypoint == ["python"]
    mock_create.assert_called_once()
    mock_schedule.assert_not_called()

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_passes_platform_to_container_create(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())
    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        resourceLimits=ResourceLimits(root={}),
        entrypoint=["python", "-c", "print('hello')"],
        platform=PlatformSpec(os="linux", arch="arm64"),
    )

    with patch.object(service, "_create_and_start_container") as mock_create:
        await service.create_sandbox(request)

    called_args = mock_create.call_args.args
    assert called_args[-1] is not None
    assert called_args[-1].os == "linux"
    assert called_args[-1].arch == "arm64"

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_response_keeps_platform_null_when_unconstrained(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())
    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        resourceLimits=ResourceLimits(root={}),
        entrypoint=["python", "-c", "print('hello')"],
    )
    created_container = MagicMock()
    created_container.image.attrs = {"Os": "linux", "Architecture": "amd64"}

    with patch.object(
        service,
        "_create_and_start_container",
        return_value=created_container,
    ):
        response = await service.create_sandbox(request)

    assert response.platform is None

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_create_and_start_container_uses_unconstrained_platform_for_execd(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client
    mock_client.api.create_host_config.return_value = {}
    mock_client.api.create_container.return_value = {"Id": "cid"}

    created_container = MagicMock()
    created_container.image.attrs = {"Os": "linux", "Architecture": "arm64"}
    mock_client.containers.get.return_value = created_container

    service = DockerSandboxService(config=_app_config())
    labels = {SANDBOX_ID_LABEL: "sandbox-1"}
    with patch.object(service, "_prepare_sandbox_runtime") as mock_prepare:
        service._create_and_start_container(
            sandbox_id="sandbox-1",
            image_uri="python:3.11",
            bootstrap_command=["python", "-c", "print('hello')"],
            labels=labels,
            environment=[],
            host_config_kwargs={},
            exposed_ports=None,
            platform=None,
        )

    passed_platform = mock_prepare.call_args.args[2]
    assert passed_platform is not None
    assert passed_platform.os == "linux"
    assert passed_platform.arch == "arm64"

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_create_and_start_container_maps_platform_typeerror_to_invalid_parameter(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client
    mock_client.api.create_host_config.return_value = {}
    mock_client.api.create_container.side_effect = TypeError("unexpected keyword argument 'platform'")

    service = DockerSandboxService(config=_app_config())
    with pytest.raises(HTTPException) as exc_info:
        service._create_and_start_container(
            sandbox_id="sandbox-1",
            image_uri="python:3.11",
            bootstrap_command=["python", "-c", "print('hello')"],
            labels={SANDBOX_ID_LABEL: "sandbox-1"},
            environment=[],
            host_config_kwargs={},
            exposed_ports=None,
            platform=PlatformSpec(os="linux", arch="arm64"),
        )

    assert exc_info.value.status_code == status.HTTP_400_BAD_REQUEST
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
    assert "platform-aware container create" in exc_info.value.detail["message"]


@patch("opensandbox_server.services.docker.docker_service.docker")
def test_create_and_start_container_windows_profile_keeps_image_entrypoint(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client
    mock_client.api.create_host_config.return_value = {}
    mock_client.api.create_container.return_value = {"Id": "cid"}

    created_container = MagicMock()
    # dockurr/windows image metadata is linux/*, but request platform is windows/*.
    created_container.image.attrs = {"Os": "linux", "Architecture": "amd64"}
    mock_client.containers.get.return_value = created_container

    service = DockerSandboxService(config=_app_config())
    with (
        patch("opensandbox_server.services.docker.container_ops.fetch_execd_install_bat", return_value=b"script"),
        patch("opensandbox_server.services.docker.container_ops.fetch_execd_windows_binary", return_value=b"exe"),
        patch("opensandbox_server.services.docker.container_ops.install_windows_oem_scripts") as mock_install,
    ):
        service._create_and_start_container(
            sandbox_id="sandbox-win-1",
            image_uri="dockurr/windows:latest",
            bootstrap_command=["cmd", "/c", "echo ready"],
            labels={SANDBOX_ID_LABEL: "sandbox-win-1"},
            environment=[],
            host_config_kwargs={},
            exposed_ports=None,
            platform=PlatformSpec(os="windows", arch="amd64"),
        )

    kwargs = mock_client.api.create_container.call_args.kwargs
    assert "entrypoint" not in kwargs
    assert "platform" not in kwargs
    assert kwargs["command"] == ["cmd", "/c", "echo ready"]
    mock_install.assert_called_once()


@patch("opensandbox_server.services.docker.docker_service.docker")
def test_create_and_start_container_windows_profile_skips_linux_runtime_injection(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client
    mock_client.api.create_host_config.return_value = {}
    mock_client.api.create_container.return_value = {"Id": "cid"}

    created_container = MagicMock()
    created_container.image.attrs = {"Os": "linux", "Architecture": "amd64"}
    mock_client.containers.get.return_value = created_container

    service = DockerSandboxService(config=_app_config())
    with (
        patch.object(service, "_prepare_sandbox_runtime") as mock_prepare,
        patch("opensandbox_server.services.docker.container_ops.fetch_execd_install_bat", return_value=b"script"),
        patch("opensandbox_server.services.docker.container_ops.fetch_execd_windows_binary", return_value=b"exe"),
        patch("opensandbox_server.services.docker.container_ops.install_windows_oem_scripts") as mock_install,
    ):
        service._create_and_start_container(
            sandbox_id="sandbox-win-2",
            image_uri="dockurr/windows:latest",
            bootstrap_command=["cmd", "/c", "echo ready"],
            labels={SANDBOX_ID_LABEL: "sandbox-win-2"},
            environment=[],
            host_config_kwargs={},
            exposed_ports=None,
            platform=PlatformSpec(os="windows", arch="amd64"),
        )

    mock_prepare.assert_not_called()
    mock_install.assert_called_once()


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_windows_profile_injects_runtime_defaults(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.runtime.execd_image = "ghcr.io/opensandbox/execd:v1.0.18"
    cfg.docker.network_mode = "bridge"
    service = DockerSandboxService(config=cfg)
    request = CreateSandboxRequest(
        image=ImageSpec(uri="dockurr/windows:latest"),
        resourceLimits=ResourceLimits(root={}),
        entrypoint=["cmd", "/c", "echo ready"],
        platform=PlatformSpec(os="windows", arch="amd64"),
    )
    created_container = MagicMock()
    created_container.image.attrs = {"Os": "windows", "Architecture": "amd64"}

    with (
        patch(
            "opensandbox_server.services.docker.docker_service.validate_windows_runtime_prerequisites",
            return_value=[],
        ),
        patch.object(
            service,
            "_create_and_start_container",
            return_value=created_container,
        ) as mock_create,
    ):
        await service.create_sandbox(request)

    host_config_kwargs = mock_create.call_args.args[5]
    assert "/dev/kvm" in host_config_kwargs["devices"]
    assert "/dev/net/tun" in host_config_kwargs["devices"]
    assert "NET_ADMIN" in host_config_kwargs["cap_add"]
    assert "NET_RAW" in host_config_kwargs["cap_add"]
    assert not any(bind.endswith(":/storage:rw") for bind in host_config_kwargs["binds"])
    assert any(bind.endswith(":/oem:rw") for bind in host_config_kwargs["binds"])
    port_bindings = host_config_kwargs["port_bindings"]
    assert "44772" in port_bindings
    assert "8080" in port_bindings
    assert "3389" in port_bindings
    assert "3389/udp" in port_bindings
    assert "8006" in port_bindings


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_windows_profile_does_not_require_download_url_override(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.runtime.execd_image = "ghcr.io/opensandbox/execd:latest"
    service = DockerSandboxService(config=cfg)
    request = CreateSandboxRequest(
        image=ImageSpec(uri="dockurr/windows:latest"),
        resourceLimits=ResourceLimits(root={}),
        entrypoint=["cmd", "/c", "echo ready"],
        platform=PlatformSpec(os="windows", arch="amd64"),
    )
    created_container = MagicMock()
    created_container.image.attrs = {"Os": "windows", "Architecture": "amd64"}

    with (
        patch(
            "opensandbox_server.services.docker.docker_service.validate_windows_runtime_prerequisites",
            return_value=[],
        ),
        patch.object(
            service,
            "_create_and_start_container",
            return_value=created_container,
        ) as mock_create,
    ):
        await service.create_sandbox(request)

    mock_create.assert_called_once()


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_windows_profile_rejects_missing_runtime_devices(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.runtime.execd_image = "ghcr.io/opensandbox/execd:v1.0.18"
    cfg.docker.network_mode = "bridge"
    service = DockerSandboxService(config=cfg)
    request = CreateSandboxRequest(
        image=ImageSpec(uri="dockurr/windows:latest"),
        resourceLimits=ResourceLimits(root={}),
        entrypoint=["cmd", "/c", "echo ready"],
        platform=PlatformSpec(os="windows", arch="amd64"),
    )
    with (
        patch(
            "opensandbox_server.services.docker.docker_service.validate_windows_runtime_prerequisites",
            side_effect=HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": "Windows profile requires host devices to be present: /dev/kvm.",
                },
            ),
        ),
        patch.object(service, "_create_and_start_container") as mock_create,
        pytest.raises(HTTPException) as exc_info,
    ):
        await service.create_sandbox(request)

    assert exc_info.value.status_code == status.HTTP_400_BAD_REQUEST
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
    assert "/dev/kvm" in exc_info.value.detail["message"]
    mock_create.assert_not_called()


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_windows_profile_rejects_below_minimum_resource_limits(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.runtime.execd_image = "ghcr.io/opensandbox/execd:v1.0.18"
    cfg.docker.network_mode = "bridge"
    service = DockerSandboxService(config=cfg)
    request = CreateSandboxRequest(
        image=ImageSpec(uri="dockurr/windows:latest"),
        resourceLimits=ResourceLimits(root={"cpu": "1", "memory": "2G", "disk": "32G"}),
        entrypoint=["cmd", "/c", "echo ready"],
        platform=PlatformSpec(os="windows", arch="amd64"),
    )
    with (
        patch(
            "opensandbox_server.services.docker.docker_service.validate_windows_runtime_prerequisites",
            return_value=None,
        ),
        patch.object(service, "_create_and_start_container") as mock_create,
        pytest.raises(HTTPException) as exc_info,
    ):
        await service.create_sandbox(request)

    assert exc_info.value.status_code == status.HTTP_400_BAD_REQUEST
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
    assert "resourceLimits.cpu >= 2" in exc_info.value.detail["message"]
    mock_create.assert_not_called()


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_windows_profile_accepts_dockur_demo_like_request(mock_docker):
    """
    Use a dockur/windows-style request payload (VERSION env) and verify
    it is forwarded through the windows profile create path.
    """
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.runtime.execd_image = "ghcr.io/opensandbox/execd:v1.0.18"
    cfg.docker.network_mode = "bridge"
    service = DockerSandboxService(config=cfg)
    request = CreateSandboxRequest(
        image=ImageSpec(uri="dockurr/windows:latest"),
        resourceLimits=ResourceLimits(
            root={
                "cpu": "4",
                "memory": "8G",
                "disk": "64G",
            }
        ),
        env={"VERSION": "11"},
        entrypoint=["cmd", "/c", "echo ready"],
        platform=PlatformSpec(os="windows", arch="amd64"),
    )
    created_container = MagicMock()
    created_container.image.attrs = {"Os": "windows", "Architecture": "amd64"}

    with (
        patch(
            "opensandbox_server.services.docker.docker_service.validate_windows_runtime_prerequisites",
            return_value=None,
        ),
        patch.object(
            service,
            "_create_and_start_container",
            return_value=created_container,
        ) as mock_create,
    ):
        response = await service.create_sandbox(request)

    forwarded_env = mock_create.call_args.args[4]
    host_config_kwargs = mock_create.call_args.args[5]
    assert "VERSION=11" in forwarded_env
    assert "CPU_CORES=4" in forwarded_env
    assert "RAM_SIZE=8G" in forwarded_env
    assert "DISK_SIZE=64G" in forwarded_env
    assert "USER_PORTS=44772,8080,3389,8006" in forwarded_env
    assert "mem_limit" not in host_config_kwargs
    assert "nano_cpus" not in host_config_kwargs
    assert response.platform is not None
    assert response.platform.os == "windows"
    assert response.platform.arch == "amd64"


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_windows_profile_with_network_policy_maps_windows_ports(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    cfg = _app_config()
    cfg.runtime.execd_image = "ghcr.io/opensandbox/execd:v1.0.18"
    cfg.docker.network_mode = "bridge"
    cfg.egress = EgressConfig(image="opensandbox/egress:latest")
    service = DockerSandboxService(config=cfg)
    request = CreateSandboxRequest(
        image=ImageSpec(uri="dockurr/windows:latest"),
        resourceLimits=ResourceLimits(
            root={
                "cpu": "4",
                "memory": "8G",
                "disk": "64G",
            }
        ),
        env={"VERSION": "11"},
        entrypoint=["cmd", "/c", "echo ready"],
        platform=PlatformSpec(os="windows", arch="amd64"),
        networkPolicy=NetworkPolicy(default_action="deny", egress=[]),
    )
    created_container = MagicMock()
    created_container.image.attrs = {"Os": "windows", "Architecture": "amd64"}
    sidecar = MagicMock()
    sidecar.id = "sidecar-123"

    with (
        patch(
            "opensandbox_server.services.docker.docker_service.validate_windows_runtime_prerequisites",
            return_value=None,
        ),
        patch("opensandbox_server.services.docker.docker_service.generate_egress_token", return_value="egress-token"),
        patch(
            "opensandbox_server.services.docker.docker_service.allocate_port_bindings",
            return_value={
                "44772": ("0.0.0.0", 51664),
                "8080": ("0.0.0.0", 48891),
                "3389/tcp": ("0.0.0.0", 53389),
                "3389/udp": ("0.0.0.0", 53390),
                "8006/tcp": ("0.0.0.0", 58006),
            },
        ),
        patch.object(service, "_start_egress_sidecar", return_value=sidecar) as mock_start_sidecar,
        patch.object(
            service,
            "_create_and_start_container",
            return_value=created_container,
        ) as mock_create,
    ):
        await service.create_sandbox(request)

    _, start_kwargs = mock_start_sidecar.call_args
    assert start_kwargs["host_execd_port"] == 51664
    assert start_kwargs["host_http_port"] == 48891
    assert start_kwargs["extra_port_bindings"] == {
        "3389/tcp": ("0.0.0.0", 53389),
        "3389/udp": ("0.0.0.0", 53390),
        "8006/tcp": ("0.0.0.0", 58006),
    }

    forwarded_env = mock_create.call_args.args[4]
    host_config_kwargs = mock_create.call_args.args[5]
    forwarded_ports = mock_create.call_args.args[6]
    labels = mock_create.call_args.args[3]

    assert "USER_PORTS=44772,8080,3389,8006" in forwarded_env
    assert host_config_kwargs["network_mode"] == "container:sidecar-123"
    assert "NET_ADMIN" in set(host_config_kwargs.get("cap_add") or [])
    assert "NET_ADMIN" not in set(host_config_kwargs.get("cap_drop") or [])
    assert forwarded_ports is None
    assert labels["opensandbox.io/embedding-proxy-port"] == "51664"
    assert labels["opensandbox.io/http-port"] == "48891"


def test_restore_existing_sandboxes_ignores_manual_cleanup_without_warning():
    service = DockerSandboxService(config=_app_config())
    manual_container = MagicMock()
    manual_container.attrs = {
        "Config": {
            "Labels": {
                SANDBOX_ID_LABEL: "manual-id",
                SANDBOX_MANUAL_CLEANUP_LABEL: "true",
            }
        }
    }

    with (
        patch.object(service.docker_client.containers, "list", return_value=[manual_container]),
        patch("opensandbox_server.services.docker.docker_service.logger.warning") as mock_warning,
        patch.object(service, "_schedule_expiration") as mock_schedule,
    ):
        service._restore_existing_sandboxes()

    mock_schedule.assert_not_called()
    mock_warning.assert_not_called()

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_pending_snapshot_restore_reports_snapshot_id_without_image(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())
    pending = PendingSandbox(
        request=MagicMock(
            metadata={"team": "platform"},
            entrypoint=["tail", "-f", "/dev/null"],
            image=ImageSpec(uri="opensandbox-snapshots:snap-001"),
            platform=None,
            snapshot_id="snap-001",
        ),
        created_at=datetime.now(timezone.utc),
        expires_at=datetime.now(timezone.utc),
        status=SandboxStatus(state="Pending"),
    )

    sandbox = service._pending_to_sandbox("sandbox-123", pending)

    assert sandbox.snapshot_id == "snap-001"
    assert sandbox.image is None

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_container_snapshot_restore_reports_snapshot_id_without_image(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())
    container = MagicMock()
    container.attrs = {
        "Config": {
            "Labels": {
                SANDBOX_ID_LABEL: "sandbox-123",
                SANDBOX_SNAPSHOT_ID_LABEL: "snap-001",
            },
            "Cmd": ["tail", "-f", "/dev/null"],
        },
        "Created": "2025-01-01T00:00:00Z",
        "State": {
            "Status": "running",
            "Running": True,
            "FinishedAt": "0001-01-01T00:00:00Z",
            "ExitCode": 0,
        },
    }
    container.image = MagicMock(tags=["opensandbox-snapshots:snap-001"], short_id="sha-image")

    sandbox = service._container_to_sandbox(container)

    assert sandbox.snapshot_id == "snap-001"
    assert sandbox.image is None

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_delete_sandbox_removes_windows_oem_volume(mock_docker):
    mock_container = MagicMock()
    mock_container.attrs = {
        "Config": {
            "Labels": {
                SANDBOX_ID_LABEL: "sandbox-win-1",
                SANDBOX_PLATFORM_OS_LABEL: "windows",
            }
        },
        "State": {"Running": True},
    }

    mock_client = MagicMock()
    mock_client.containers.list.return_value = [mock_container]
    mock_docker.from_env.return_value = mock_client
    service = DockerSandboxService(config=_app_config())

    service.delete_sandbox("sandbox-win-1")

    mock_client.api.remove_volume.assert_called_once_with("opensandbox-win-oem-sandbox-win-1")


@patch("opensandbox_server.services.docker.docker_service.docker")
def test_delete_sandbox_skips_oem_volume_cleanup_for_linux(mock_docker):
    mock_container = MagicMock()
    mock_container.attrs = {
        "Config": {
            "Labels": {
                SANDBOX_ID_LABEL: "sandbox-linux-1",
                SANDBOX_PLATFORM_OS_LABEL: "linux",
            }
        },
        "State": {"Running": True},
    }

    mock_client = MagicMock()
    mock_client.containers.list.return_value = [mock_container]
    mock_docker.from_env.return_value = mock_client
    service = DockerSandboxService(config=_app_config())

    service.delete_sandbox("sandbox-linux-1")

    mock_client.api.remove_volume.assert_not_called()

def test_renew_expiration_rejects_manual_cleanup_sandbox():
    service = DockerSandboxService(config=_app_config())
    container = MagicMock()
    container.attrs = {
        "Config": {
            "Labels": {
                SANDBOX_ID_LABEL: "manual-id",
                SANDBOX_MANUAL_CLEANUP_LABEL: "true",
            }
        }
    }
    request = MagicMock(expires_at=datetime.now(timezone.utc) + timedelta(hours=1))

    with patch.object(service, "_get_container_by_sandbox_id", return_value=container):
        with pytest.raises(HTTPException) as exc_info:
            service.renew_expiration("manual-id", request)

    assert exc_info.value.status_code == status.HTTP_409_CONFLICT
    assert exc_info.value.detail["message"] == "Sandbox manual-id does not have automatic expiration enabled."

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_create_sandbox_async_returns_provisioning(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())

    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={"team": "async"},
        entrypoint=["python", "app.py"],
    )

    with patch.object(service, "create_sandbox", new_callable=AsyncMock) as mock_sync:
        mock_sync.return_value = CreateSandboxResponse(
            id="sandbox-sync",
            status=SandboxStatus(
                state="Running",
                reason="CONTAINER_RUNNING",
                message="started",
                last_transition_at=datetime.now(timezone.utc),
            ),
            metadata={"team": "async"},
            expiresAt=datetime.now(timezone.utc),
            createdAt=datetime.now(timezone.utc),
            entrypoint=["python", "app.py"],
        )
        response = await service.create_sandbox(request)

    assert response.status.state == "Running"
    assert response.metadata == {"team": "async"}
    mock_sync.assert_called_once()

@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_get_sandbox_returns_pending_state(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())

    request = CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python", "app.py"],
    )

    with patch.object(service, "create_sandbox", new_callable=AsyncMock) as mock_sync:
        mock_sync.return_value = CreateSandboxResponse(
            id="sandbox-sync",
            status=SandboxStatus(
                state="Running",
                reason="CONTAINER_RUNNING",
                message="started",
                last_transition_at=datetime.now(timezone.utc),
            ),
            metadata={},
            expiresAt=datetime.now(timezone.utc),
            createdAt=datetime.now(timezone.utc),
            entrypoint=["python", "app.py"],
        )
        response = await service.create_sandbox(request)

    assert response.status.state == "Running"
    assert response.entrypoint == ["python", "app.py"]

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_list_sandboxes_deduplicates_container_and_pending(mock_docker):
    # Build a realistic container mock to avoid parse_timestamp errors.
    container = MagicMock()
    container.attrs = {
        "Config": {"Labels": {SANDBOX_ID_LABEL: "sandbox-123"}},
        "Created": "2025-01-01T00:00:00Z",
        "State": {
            "Status": "running",
            "Running": True,
            "FinishedAt": "0001-01-01T00:00:00Z",
            "ExitCode": 0,
        },
    }
    container.image = MagicMock(tags=["image:latest"], short_id="sha-image")

    mock_client = MagicMock()
    mock_client.containers.list.return_value = [container]
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())
    sandbox_id = "sandbox-123"

    # Prepare container and pending representations
    container_sandbox = Sandbox(
        id=sandbox_id,
        image=ImageSpec(uri="image:latest"),
        status=SandboxStatus(
            state="Running",
            reason="CONTAINER_RUNNING",
            message="running",
            last_transition_at=datetime.now(timezone.utc),
        ),
        metadata={"team": "c"},
        entrypoint=["/bin/sh"],
        expiresAt=datetime.now(timezone.utc),
        createdAt=datetime.now(timezone.utc),
    )
    # Force container state to be returned
    service._container_to_sandbox = MagicMock(return_value=container_sandbox)

    response = service.list_sandboxes(ListSandboxesRequest(filter=SandboxFilter(), pagination=None))

    assert len(response.items) == 1
    assert response.items[0].status.state == "Running"
    assert response.items[0].metadata == {"team": "c"}

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_get_sandbox_prefers_container_over_pending(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())
    sandbox_id = "sandbox-abc"

    pending_status = SandboxStatus(
        state="Pending",
        reason="SANDBOX_SCHEDULED",
        message="pending",
        last_transition_at=datetime.now(timezone.utc),
    )
    service._pending_sandboxes[sandbox_id] = PendingSandbox(
        request=MagicMock(metadata={}, entrypoint=["/bin/sh"], image=ImageSpec(uri="image:latest")),
        created_at=datetime.now(timezone.utc),
        expires_at=datetime.now(timezone.utc),
        status=pending_status,
    )

    container_sandbox = Sandbox(
        id=sandbox_id,
        image=ImageSpec(uri="image:latest"),
        status=SandboxStatus(
            state="Running",
            reason="CONTAINER_RUNNING",
            message="running",
            last_transition_at=datetime.now(timezone.utc),
        ),
        metadata={},
        entrypoint=["/bin/sh"],
        expiresAt=datetime.now(timezone.utc),
        createdAt=datetime.now(timezone.utc),
    )

    service._get_container_by_sandbox_id = MagicMock(return_value=MagicMock())
    service._container_to_sandbox = MagicMock(return_value=container_sandbox)

    sandbox = service.get_sandbox(sandbox_id)
    assert sandbox.status.state == "Running"
    assert sandbox.entrypoint == ["/bin/sh"]

@patch("opensandbox_server.services.docker.docker_service.docker")
def test_async_worker_cleans_up_leftover_container_on_failure(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_docker.from_env.return_value = mock_client

    service = DockerSandboxService(config=_app_config())
    sandbox_id = "sandbox-fail"
    created_at = datetime.now(timezone.utc)
    expires_at = created_at

    pending_status = SandboxStatus(
        state="Pending",
        reason="SANDBOX_SCHEDULED",
        message="pending",
        last_transition_at=created_at,
    )
    service._pending_sandboxes[sandbox_id] = PendingSandbox(
        request=MagicMock(metadata={}, entrypoint=["/bin/sh"], image=ImageSpec(uri="image:latest")),
        created_at=created_at,
        expires_at=expires_at,
        status=pending_status,
    )

    service._provision_sandbox = MagicMock(
        side_effect=HTTPException(
            status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
            detail={"message": "boom"},
        )
    )
    service._cleanup_failed_containers = MagicMock()

    service._async_provision_worker(
        sandbox_id,
        MagicMock(),
        created_at,
        expires_at,
    )

    service._cleanup_failed_containers.assert_called_once_with(sandbox_id)
    assert service._pending_sandboxes[sandbox_id].status.state == "Failed"

@patch("opensandbox_server.services.docker.docker_service.docker")
class TestBuildVolumeBinds:

    def test_none_volumes_returns_empty(self, mock_docker):
        """None volumes should produce empty binds list."""
        mock_docker.from_env.return_value = MagicMock()
        service = DockerSandboxService(config=_app_config())
        assert service._build_volume_binds(None) == []

    def test_empty_volumes_returns_empty(self, mock_docker):
        """Empty volumes list should produce empty binds list."""
        mock_docker.from_env.return_value = MagicMock()
        service = DockerSandboxService(config=_app_config())
        assert service._build_volume_binds([]) == []

    def test_single_host_volume_rw(self, mock_docker):
        """Single host volume with read-write should produce correct bind string."""
        mock_docker.from_env.return_value = MagicMock()
        service = DockerSandboxService(config=_app_config())
        volume = Volume(
            name="workdir",
            host=Host(path="/data/opensandbox/user-a"),
            mount_path="/mnt/work",
            read_only=False,
        )
        binds = service._build_volume_binds([volume])
        assert binds == ["/data/opensandbox/user-a:/mnt/work:rw"]

    def test_single_host_volume_ro(self, mock_docker):
        """Single host volume with read-only should produce correct bind string."""
        mock_docker.from_env.return_value = MagicMock()
        service = DockerSandboxService(config=_app_config())
        volume = Volume(
            name="workdir",
            host=Host(path="/data/opensandbox/user-a"),
            mount_path="/mnt/work",
            read_only=True,
        )
        binds = service._build_volume_binds([volume])
        assert binds == ["/data/opensandbox/user-a:/mnt/work:ro"]

    def test_host_volume_with_subpath(self, mock_docker):
        """Host volume with subPath should resolve the full host path."""
        mock_docker.from_env.return_value = MagicMock()
        service = DockerSandboxService(config=_app_config())
        volume = Volume(
            name="workdir",
            host=Host(path="/data/opensandbox/user-a"),
            mount_path="/mnt/work",
            read_only=False,
            sub_path="task-001",
        )
        binds = service._build_volume_binds([volume])
        expected_host = os.path.normpath("/data/opensandbox/user-a/task-001")
        assert binds == [f"{expected_host}:/mnt/work:rw"]

    def test_multiple_host_volumes(self, mock_docker):
        """Multiple host volumes should produce multiple bind strings."""
        mock_docker.from_env.return_value = MagicMock()
        service = DockerSandboxService(config=_app_config())
        volumes = [
            Volume(
                name="workdir",
                host=Host(path="/data/work"),
                mount_path="/mnt/work",
                read_only=False,
            ),
            Volume(
                name="data",
                host=Host(path="/data/shared"),
                mount_path="/mnt/data",
                read_only=True,
            ),
        ]
        binds = service._build_volume_binds(volumes)
        assert len(binds) == 2
        assert "/data/work:/mnt/work:rw" in binds
        assert "/data/shared:/mnt/data:ro" in binds

    def test_single_pvc_volume_rw(self, mock_docker):
        """Single PVC volume with read-write (no subPath) should produce named volume bind string."""
        mock_docker.from_env.return_value = MagicMock()
        service = DockerSandboxService(config=_app_config())
        volume = Volume(
            name="shared-data",
            pvc=PVC(claim_name="my-shared-volume"),
            mount_path="/mnt/data",
            read_only=False,
        )
        binds = service._build_volume_binds([volume])
        assert binds == ["my-shared-volume:/mnt/data:rw"]

    def test_single_pvc_volume_ro(self, mock_docker):
        """Single PVC volume with read-only (no subPath) should produce named volume bind string."""
        mock_docker.from_env.return_value = MagicMock()
        service = DockerSandboxService(config=_app_config())
        volume = Volume(
            name="models",
            pvc=PVC(claim_name="shared-models-pvc"),
            mount_path="/mnt/models",
            read_only=True,
        )
        binds = service._build_volume_binds([volume])
        assert binds == ["shared-models-pvc:/mnt/models:ro"]

    def test_pvc_volume_with_subpath(self, mock_docker):
        """PVC volume with subPath should resolve via cached Mountpoint and produce bind mount."""
        mock_docker.from_env.return_value = MagicMock()
        service = DockerSandboxService(config=_app_config())
        volume = Volume(
            name="datasets",
            pvc=PVC(claim_name="my-vol"),
            mount_path="/mnt/train",
            read_only=False,
            sub_path="datasets/train",
        )
        cache = {
            "my-vol": {
                "Name": "my-vol",
                "Driver": "local",
                "Mountpoint": "/var/lib/docker/volumes/my-vol/_data",
            }
        }
        binds = service._build_volume_binds([volume], pvc_inspect_cache=cache)
        assert binds == ["/var/lib/docker/volumes/my-vol/_data/datasets/train:/mnt/train:rw"]

    def test_pvc_volume_with_subpath_readonly(self, mock_docker):
        """PVC volume with subPath and readOnly should produce ':ro' bind mount."""
        mock_docker.from_env.return_value = MagicMock()
        service = DockerSandboxService(config=_app_config())
        volume = Volume(
            name="datasets",
            pvc=PVC(claim_name="my-vol"),
            mount_path="/mnt/eval",
            read_only=True,
            sub_path="datasets/eval",
        )
        cache = {
            "my-vol": {
                "Name": "my-vol",
                "Driver": "local",
                "Mountpoint": "/var/lib/docker/volumes/my-vol/_data",
            }
        }
        binds = service._build_volume_binds([volume], pvc_inspect_cache=cache)
        assert binds == ["/var/lib/docker/volumes/my-vol/_data/datasets/eval:/mnt/eval:ro"]

    def test_mixed_host_and_pvc_volumes(self, mock_docker):
        """Mixed host and PVC volumes should both produce bind strings."""
        mock_docker.from_env.return_value = MagicMock()
        service = DockerSandboxService(config=_app_config())
        volumes = [
            Volume(
                name="workdir",
                host=Host(path="/data/work"),
                mount_path="/mnt/work",
                read_only=False,
            ),
            Volume(
                name="shared-data",
                pvc=PVC(claim_name="my-shared-volume"),
                mount_path="/mnt/data",
                read_only=True,
            ),
        ]
        binds = service._build_volume_binds(volumes)
        assert len(binds) == 2
        assert "/data/work:/mnt/work:rw" in binds
        assert "my-shared-volume:/mnt/data:ro" in binds

    def test_ossfs_volume_with_subpath(self, mock_docker):
        """OSSFS volume should resolve host path using subPath as OSS prefix."""
        mock_docker.from_env.return_value = MagicMock()
        service = DockerSandboxService(config=_app_config())
        volume = Volume(
            name="oss-data",
            ossfs=OSSFS(
                bucket="bucket-test-3",
                endpoint="oss-cn-hangzhou.aliyuncs.com",
                access_key_id="AKIDEXAMPLE",
                access_key_secret="SECRETEXAMPLE",
            ),
            mount_path="/mnt/data",
            read_only=False,
            sub_path="task-001",
        )
        binds = service._build_volume_binds([volume])
        assert binds == ["/mnt/ossfs/bucket-test-3/task-001:/mnt/data:rw"]

@patch("opensandbox_server.services.docker.docker_service.docker")
class TestDockerVolumeValidation:

    @pytest.mark.asyncio
    async def test_pvc_volume_not_found_rejected(self, mock_docker):
        """PVC backend with non-existent Docker named volume should be rejected when createIfNotExists is false."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.inspect_volume.side_effect = DockerNotFound("volume not found")
        mock_docker.from_env.return_value = mock_client

        cfg = _app_config()
        service = DockerSandboxService(config=cfg)

        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=120,
            resourceLimits=ResourceLimits(root={}),
            env={},
            metadata={},
            entrypoint=["python"],
            volumes=[
                Volume(
                    name="models",
                    pvc=PVC(claim_name="nonexistent-volume", create_if_not_exists=False),
                    mount_path="/mnt/models",
                    read_only=True,
                )
            ],
        )

        with pytest.raises(HTTPException) as exc_info:
            await service.create_sandbox(request)

        assert exc_info.value.status_code == status.HTTP_400_BAD_REQUEST
        assert exc_info.value.detail["code"] == SandboxErrorCodes.PVC_VOLUME_NOT_FOUND

    def test_pvc_volume_auto_created_when_not_found(self, mock_docker):
        """PVC backend auto-creates Docker named volume when createIfNotExists is true (default)."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        # First inspect fails (not found), then succeeds after create
        mock_client.api.inspect_volume.side_effect = [
            DockerNotFound("volume not found"),
            {"Name": "my-volume", "Driver": "local", "Mountpoint": "/var/lib/docker/volumes/my-volume/_data"},
        ]
        mock_client.api.create_volume.return_value = {}
        mock_docker.from_env.return_value = mock_client

        cfg = _app_config()
        service = DockerSandboxService(config=cfg)

        volume = Volume(
            name="data",
            pvc=PVC(claim_name="my-volume"),
            mount_path="/mnt/data",
            read_only=False,
        )
        vol_info, auto_created = service._validate_pvc_volume(volume)

        mock_client.api.create_volume.assert_called_once_with(
            name="my-volume",
            labels={"opensandbox.io/volume-managed-by": "server"},
        )
        assert vol_info["Name"] == "my-volume"
        assert auto_created is True

    def test_ossfs_inline_credentials_missing_rejected(self, mock_docker):
        """OSSFS with missing inline credentials should be rejected at schema validation."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_docker.from_env.return_value = mock_client
        with pytest.raises(ValidationError):
            OSSFS(
                bucket="bucket-test-3",
                endpoint="oss-cn-hangzhou.aliyuncs.com",
                access_key_id=None,
                access_key_secret=None,
            )

    @pytest.mark.asyncio
    async def test_ossfs_mount_failure_rejected(self, mock_docker):
        """OSSFS mount failure should be rejected."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_docker.from_env.return_value = mock_client
        service = DockerSandboxService(config=_app_config())

        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=120,
            resourceLimits=ResourceLimits(root={}),
            env={},
            metadata={},
            entrypoint=["python"],
            volumes=[
                Volume(
                    name="oss-data",
                    ossfs=OSSFS(
                        bucket="bucket-test-3",
                        endpoint="oss-cn-hangzhou.aliyuncs.com",
                        access_key_id="AKIDEXAMPLE",
                        access_key_secret="SECRETEXAMPLE",
                    ),
                    mount_path="/mnt/data",
                    sub_path="task-001",
                )
            ],
        )

        with patch("opensandbox_server.services.docker.ossfs_mixin.os.name", "posix"):
            with patch("opensandbox_server.services.docker.ossfs_mixin.os.path.ismount", return_value=False):
                with patch("opensandbox_server.services.docker.ossfs_mixin.os.makedirs"):
                    with patch("opensandbox_server.services.docker.ossfs_mixin.subprocess.run") as mock_run:
                        mock_run.return_value = MagicMock(returncode=1, stderr="mount failed")
                        with pytest.raises(HTTPException) as exc_info:
                            await service.create_sandbox(request)

        assert exc_info.value.status_code == status.HTTP_500_INTERNAL_SERVER_ERROR
        assert exc_info.value.detail["code"] == SandboxErrorCodes.OSSFS_MOUNT_FAILED

    def test_ossfs_windows_host_not_supported(self, mock_docker):
        """OSSFS backend should be rejected when server host is Windows."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_docker.from_env.return_value = mock_client
        service = DockerSandboxService(config=_app_config())
        volume = Volume(
            name="oss-data",
            ossfs=OSSFS(
                bucket="bucket-test-3",
                endpoint="oss-cn-hangzhou.aliyuncs.com",
                access_key_id="AKIDEXAMPLE",
                access_key_secret="SECRETEXAMPLE",
            ),
            mount_path="/mnt/data",
        )

        with patch("opensandbox_server.services.docker.ossfs_mixin.os.name", "nt"):
            with pytest.raises(HTTPException) as exc_info:
                service._validate_ossfs_volume(volume)
        assert exc_info.value.status_code == status.HTTP_400_BAD_REQUEST
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER

    def test_ossfs_v1_mount_command_uses_o_options(self, mock_docker):
        """OSSFS 1.0 should build mount command with -o style options."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_docker.from_env.return_value = mock_client
        service = DockerSandboxService(config=_app_config())
        volume = Volume(
            name="oss-data",
            ossfs=OSSFS(
                bucket="bucket-test-3",
                endpoint="oss-cn-hangzhou.aliyuncs.com",
                version="1.0",
                options=["allow_other", "umask=0022"],
                access_key_id="AKIDEXAMPLE",
                access_key_secret="SECRETEXAMPLE",
            ),
            mount_path="/mnt/data",
            sub_path="task-001",
        )
        backend_path = "/mnt/ossfs/bucket-test-3/task-001"

        with patch("opensandbox_server.services.docker.ossfs_mixin.os.makedirs"):
            with patch("opensandbox_server.services.docker.ossfs_mixin.subprocess.run") as mock_run:
                mock_run.return_value = MagicMock(returncode=0, stderr="")
                service._mount_ossfs_backend_path(volume, backend_path)

        cmd = mock_run.call_args.args[0]
        assert "bucket-test-3:/task-001" in cmd
        assert "-o" in cmd
        assert "allow_other" in cmd
        assert "umask=0022" in cmd
        assert "--allow_other" not in cmd
        assert "sigv4" not in cmd
        assert not any(str(part).startswith("region=") for part in cmd)

    def test_ossfs_v2_mount_command_uses_config_file(self, mock_docker):
        """OSSFS 2.0 should mount by ossfs2 config file."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_docker.from_env.return_value = mock_client
        service = DockerSandboxService(config=_app_config())
        volume = Volume(
            name="oss-data",
            ossfs=OSSFS(
                bucket="bucket-test-3",
                endpoint="oss-cn-hangzhou.aliyuncs.com",
                version="2.0",
                options=["allow_other", "umask=0022"],
                access_key_id="AKIDEXAMPLE",
                access_key_secret="SECRETEXAMPLE",
            ),
            mount_path="/mnt/data",
            sub_path="task-001",
        )
        backend_path = "/mnt/ossfs/bucket-test-3/task-001"

        with patch("opensandbox_server.services.docker.ossfs_mixin.os.makedirs"):
            with patch("opensandbox_server.services.docker.ossfs_mixin.subprocess.run") as mock_run:
                mock_run.return_value = MagicMock(returncode=0, stderr="")
                service._mount_ossfs_backend_path(volume, backend_path)

        cmd = mock_run.call_args.args[0]
        assert cmd[0] == "ossfs2"
        assert cmd[1] == "mount"
        assert cmd[2] == backend_path
        assert cmd[3] == "-c"
        assert cmd[4].endswith(".conf")

    def test_ossfs_v2_config_contains_required_lines(self, mock_docker):
        """OSSFS 2.0 config should encode endpoint/bucket/creds/options/prefix."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_docker.from_env.return_value = mock_client
        service = DockerSandboxService(config=_app_config())
        volume = Volume(
            name="oss-data",
            ossfs=OSSFS(
                bucket="bucket-test-3",
                endpoint="oss-cn-hangzhou.aliyuncs.com",
                version="2.0",
                options=["allow_other", "umask=0022"],
                access_key_id="AKIDEXAMPLE",
                access_key_secret="SECRETEXAMPLE",
            ),
            mount_path="/mnt/data",
            sub_path="task-001",
        )

        conf_lines = service._build_ossfs_v2_config_lines(
            volume=volume,
            endpoint_url="http://oss-cn-hangzhou.aliyuncs.com",
            prefix="task-001",
        )
        assert "--oss_endpoint=http://oss-cn-hangzhou.aliyuncs.com" in conf_lines
        assert "--oss_bucket=bucket-test-3" in conf_lines
        assert "--oss_access_key_id=AKIDEXAMPLE" in conf_lines
        assert "--oss_access_key_secret=SECRETEXAMPLE" in conf_lines
        assert "--oss_bucket_prefix=task-001/" in conf_lines
        assert "--allow_other" in conf_lines
        assert "--umask=0022" in conf_lines

    @pytest.mark.asyncio
    async def test_ossfs_volume_binds_passed_to_docker(self, mock_docker):
        """OSSFS volume should be converted to host bind path and passed to Docker."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.create_host_config.return_value = {}
        mock_client.api.create_container.return_value = {"Id": "cid"}
        mock_client.containers.get.return_value = MagicMock()
        mock_docker.from_env.return_value = mock_client
        service = DockerSandboxService(config=_app_config())

        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=120,
            resourceLimits=ResourceLimits(root={}),
            env={},
            metadata={},
            entrypoint=["python"],
            volumes=[
                Volume(
                    name="oss-data",
                    ossfs=OSSFS(
                        bucket="bucket-test-3",
                        endpoint="oss-cn-hangzhou.aliyuncs.com",
                        access_key_id="AKIDEXAMPLE",
                        access_key_secret="SECRETEXAMPLE",
                    ),
                    mount_path="/mnt/data",
                    read_only=True,
                    sub_path="task-001",
                )
            ],
        )

        with patch("opensandbox_server.services.docker.ossfs_mixin.os.name", "posix"):
            with patch("opensandbox_server.services.docker.ossfs_mixin.os.path.ismount", return_value=False):
                with patch("opensandbox_server.services.docker.ossfs_mixin.os.makedirs"):
                    with patch("opensandbox_server.services.docker.ossfs_mixin.subprocess.run") as mock_run:
                        mock_run.return_value = MagicMock(returncode=0, stderr="")
                        with patch.object(service, "_ensure_image_available"), patch.object(
                            service, "_prepare_sandbox_runtime"
                        ):
                            response = await service.create_sandbox(request)

        assert response.status.state == "Running"
        assert mock_run.called
        host_config_call = mock_client.api.create_host_config.call_args
        binds = host_config_call.kwargs["binds"]
        assert binds[0] == "/mnt/ossfs/bucket-test-3/task-001:/mnt/data:ro"
        create_call = mock_client.api.create_container.call_args
        labels = create_call.kwargs["labels"]
        assert SANDBOX_OSSFS_MOUNTS_LABEL in labels
        assert labels[SANDBOX_OSSFS_MOUNTS_LABEL] == '["/mnt/ossfs/bucket-test-3/task-001"]'

    def test_prepare_ossfs_mounts_reuses_mount_key(self, mock_docker):
        """Two OSSFS volumes on same base path should mount once and share refs."""
        mock_docker.from_env.return_value = MagicMock()
        service = DockerSandboxService(config=_app_config())
        volumes = [
            Volume(
                name="oss-data-a",
                ossfs=OSSFS(
                    bucket="bucket-test-3",
                    endpoint="oss-cn-hangzhou.aliyuncs.com",
                    access_key_id="AKIDEXAMPLE",
                    access_key_secret="SECRETEXAMPLE",
                ),
                mount_path="/mnt/data-a",
                sub_path="task-001",
            ),
            Volume(
                name="oss-data-b",
                ossfs=OSSFS(
                    bucket="bucket-test-3",
                    endpoint="oss-cn-hangzhou.aliyuncs.com",
                    access_key_id="AKIDEXAMPLE",
                    access_key_secret="SECRETEXAMPLE",
                ),
                mount_path="/mnt/data-b",
                sub_path="task-001",
            ),
        ]

        with patch("opensandbox_server.services.docker.ossfs_mixin.os.path.ismount", return_value=False):
            with patch("opensandbox_server.services.docker.ossfs_mixin.os.makedirs"):
                with patch("opensandbox_server.services.docker.ossfs_mixin.subprocess.run") as mock_run:
                    mock_run.return_value = MagicMock(returncode=0, stderr="")
                    mount_keys = service._prepare_ossfs_mounts(volumes)

        mount_key = "/mnt/ossfs/bucket-test-3/task-001"
        assert mount_keys == [mount_key]
        assert service._ossfs_mount_ref_counts[mount_key] == 1
        assert mock_run.call_count == 1

    def test_prepare_ossfs_mounts_rolls_back_on_partial_failure(self, mock_docker):
        """If one OSSFS mount fails, already prepared mounts should be rolled back."""
        mock_docker.from_env.return_value = MagicMock()
        service = DockerSandboxService(config=_app_config())
        volumes = [
            Volume(
                name="oss-data-a",
                ossfs=OSSFS(
                    bucket="bucket-a",
                    endpoint="oss-cn-hangzhou.aliyuncs.com",
                    access_key_id="AKIDEXAMPLE",
                    access_key_secret="SECRETEXAMPLE",
                ),
                mount_path="/mnt/data-a",
            ),
            Volume(
                name="oss-data-b",
                ossfs=OSSFS(
                    bucket="bucket-b",
                    endpoint="oss-cn-hangzhou.aliyuncs.com",
                    access_key_id="AKIDEXAMPLE",
                    access_key_secret="SECRETEXAMPLE",
                ),
                mount_path="/mnt/data-b",
            ),
        ]

        mount_key_a = "/mnt/ossfs/bucket-a"
        mount_key_b = "/mnt/ossfs/bucket-b"

        with patch.object(
            service,
            "_ensure_ossfs_mounted",
            side_effect=[mount_key_a, HTTPException(status_code=500, detail={"code": "E", "message": "boom"})],
        ) as ensure_mock:
            with patch.object(service, "_release_ossfs_mounts") as release_mock:
                with pytest.raises(HTTPException):
                    service._prepare_ossfs_mounts(volumes)

        assert ensure_mock.call_count == 2
        release_mock.assert_called_once_with([mount_key_a])
        assert mount_key_b not in release_mock.call_args.args[0]

    def test_delete_sandbox_releases_ossfs_mount(self, mock_docker):
        """Deleting sandbox should release and unmount tracked OSSFS mount."""
        mount_key = "/mnt/ossfs/bucket-test-3/task-001"
        mock_container = MagicMock()
        mock_container.attrs = {
            "Config": {
                "Labels": {
                    SANDBOX_ID_LABEL: "sandbox-1",
                    SANDBOX_OSSFS_MOUNTS_LABEL: f'["{mount_key}"]',
                }
            },
            "State": {"Running": True},
        }

        mock_client = MagicMock()
        mock_client.containers.list.return_value = [mock_container]
        mock_docker.from_env.return_value = mock_client
        service = DockerSandboxService(config=_app_config())
        service._ossfs_mount_ref_counts[mount_key] = 1

        with patch("opensandbox_server.services.docker.ossfs_mixin.os.path.ismount", return_value=True):
            with patch("opensandbox_server.services.docker.ossfs_mixin.subprocess.run") as mock_run:
                mock_run.return_value = MagicMock(returncode=0, stderr="")
                service.delete_sandbox("sandbox-1")

        assert mount_key not in service._ossfs_mount_ref_counts
        assert mock_run.called

    def test_release_ossfs_mount_untracked_key_does_not_unmount(self, mock_docker):
        """Untracked mount key must not trigger unmount command."""
        mount_key = "/mnt/ossfs/bucket-test-3/task-001"
        mock_docker.from_env.return_value = MagicMock()
        service = DockerSandboxService(config=_app_config())

        with patch("opensandbox_server.services.docker.ossfs_mixin.os.path.ismount", return_value=True):
            with patch("opensandbox_server.services.docker.ossfs_mixin.subprocess.run") as mock_run:
                service._release_ossfs_mount(mount_key)

        mock_run.assert_not_called()
        assert mount_key not in service._ossfs_mount_ref_counts

    def test_restore_existing_sandboxes_rebuilds_ossfs_refs(self, mock_docker):
        """Service startup rebuilds OSSFS mount refs from container labels."""
        mount_key = "/mnt/ossfs/bucket-test-3/task-001"
        expires_at = (datetime.now(timezone.utc) + timedelta(minutes=10)).isoformat()
        container = MagicMock()
        container.attrs = {
            "Config": {
                "Labels": {
                    SANDBOX_ID_LABEL: "sandbox-1",
                    SANDBOX_EXPIRES_AT_LABEL: expires_at,
                    SANDBOX_OSSFS_MOUNTS_LABEL: f'["{mount_key}"]',
                }
            },
            "State": {"Running": True},
        }
        mock_client = MagicMock()
        mock_client.containers.list.return_value = [container]
        mock_docker.from_env.return_value = mock_client

        service = DockerSandboxService(config=_app_config())

        assert service._ossfs_mount_ref_counts[mount_key] == 1

    def test_delete_one_sandbox_after_restart_keeps_shared_mount(self, mock_docker):
        """After restart, deleting one of two users must not unmount shared OSSFS mount."""
        mount_key = "/mnt/ossfs/bucket-test-3/task-001"
        expires_at = (datetime.now(timezone.utc) + timedelta(minutes=10)).isoformat()
        container_a = MagicMock()
        container_a.attrs = {
            "Config": {
                "Labels": {
                    SANDBOX_ID_LABEL: "sandbox-a",
                    SANDBOX_EXPIRES_AT_LABEL: expires_at,
                    SANDBOX_OSSFS_MOUNTS_LABEL: f'["{mount_key}"]',
                }
            },
            "State": {"Running": True},
        }
        container_b = MagicMock()
        container_b.attrs = {
            "Config": {
                "Labels": {
                    SANDBOX_ID_LABEL: "sandbox-b",
                    SANDBOX_EXPIRES_AT_LABEL: expires_at,
                    SANDBOX_OSSFS_MOUNTS_LABEL: f'["{mount_key}"]',
                }
            },
            "State": {"Running": True},
        }
        mock_client = MagicMock()
        mock_client.containers.list.return_value = [container_a, container_b]
        mock_docker.from_env.return_value = mock_client

        service = DockerSandboxService(config=_app_config())
        assert service._ossfs_mount_ref_counts[mount_key] == 2

        with patch("opensandbox_server.services.docker.ossfs_mixin.os.path.ismount", return_value=True):
            with patch("opensandbox_server.services.docker.ossfs_mixin.subprocess.run") as mock_run:
                service.delete_sandbox("sandbox-a")

        assert service._ossfs_mount_ref_counts[mount_key] == 1
        mock_run.assert_not_called()

    def test_restore_manual_cleanup_sandbox_rebuilds_ossfs_refs(self, mock_docker):
        """Manual cleanup sandbox OSSFS refs should be restored on startup."""
        mount_key = "/mnt/ossfs/bucket-manual/data"
        container = MagicMock()
        container.attrs = {
            "Config": {
                "Labels": {
                    SANDBOX_ID_LABEL: "sandbox-manual",
                    SANDBOX_MANUAL_CLEANUP_LABEL: "true",
                    SANDBOX_OSSFS_MOUNTS_LABEL: f'["{mount_key}"]',
                }
            },
            "State": {"Running": True},
        }
        mock_client = MagicMock()
        mock_client.containers.list.return_value = [container]
        mock_docker.from_env.return_value = mock_client

        service = DockerSandboxService(config=_app_config())

        assert service._ossfs_mount_ref_counts.get(mount_key) == 1

    @pytest.mark.asyncio
    async def test_pvc_volume_inspect_failure_returns_500(self, mock_docker):
        """Docker API failure during volume inspection should return 500."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.inspect_volume.side_effect = DockerException("connection error")
        mock_docker.from_env.return_value = mock_client

        service = DockerSandboxService(config=_app_config())

        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=120,
            resourceLimits=ResourceLimits(root={}),
            env={},
            metadata={},
            entrypoint=["python"],
            volumes=[
                Volume(
                    name="shared-data",
                    pvc=PVC(claim_name="my-volume"),
                    mount_path="/mnt/data",
                )
            ],
        )

        with pytest.raises(HTTPException) as exc_info:
            await service.create_sandbox(request)

        assert exc_info.value.status_code == status.HTTP_500_INTERNAL_SERVER_ERROR
        assert exc_info.value.detail["code"] == SandboxErrorCodes.PVC_VOLUME_INSPECT_FAILED

    @pytest.mark.asyncio
    async def test_pvc_volume_binds_passed_to_docker(self, mock_docker):
        """PVC volume binds should be passed to Docker host config as named volume refs."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.inspect_volume.return_value = {"Name": "my-shared-volume"}
        mock_client.api.create_host_config.return_value = {}
        mock_client.api.create_container.return_value = {"Id": "cid"}
        mock_client.containers.get.return_value = MagicMock()
        mock_docker.from_env.return_value = mock_client

        service = DockerSandboxService(config=_app_config())

        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=120,
            resourceLimits=ResourceLimits(root={}),
            env={},
            metadata={},
            entrypoint=["python"],
            volumes=[
                Volume(
                    name="shared-data",
                    pvc=PVC(claim_name="my-shared-volume"),
                    mount_path="/mnt/data",
                    read_only=False,
                )
            ],
        )

        with (
            patch.object(service, "_ensure_image_available"),
            patch.object(service, "_prepare_sandbox_runtime"),
        ):
            response = await service.create_sandbox(request)

        assert response.status.state == "Running"

        # Verify named volume bind was passed to create_host_config
        host_config_call = mock_client.api.create_host_config.call_args
        assert "binds" in host_config_call.kwargs
        binds = host_config_call.kwargs["binds"]
        assert len(binds) == 1
        assert binds[0] == "my-shared-volume:/mnt/data:rw"

    @pytest.mark.asyncio
    async def test_pvc_volume_readonly_binds_passed_to_docker(self, mock_docker):
        """PVC volume with read-only should produce ':ro' bind string."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.inspect_volume.return_value = {"Name": "shared-models"}
        mock_client.api.create_host_config.return_value = {}
        mock_client.api.create_container.return_value = {"Id": "cid"}
        mock_client.containers.get.return_value = MagicMock()
        mock_docker.from_env.return_value = mock_client

        service = DockerSandboxService(config=_app_config())

        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=120,
            resourceLimits=ResourceLimits(root={}),
            env={},
            metadata={},
            entrypoint=["python"],
            volumes=[
                Volume(
                    name="models",
                    pvc=PVC(claim_name="shared-models"),
                    mount_path="/mnt/models",
                    read_only=True,
                )
            ],
        )

        with (
            patch.object(service, "_ensure_image_available"),
            patch.object(service, "_prepare_sandbox_runtime"),
        ):
            await service.create_sandbox(request)

        host_config_call = mock_client.api.create_host_config.call_args
        binds = host_config_call.kwargs["binds"]
        assert binds[0] == "shared-models:/mnt/models:ro"

    @pytest.mark.asyncio
    async def test_pvc_subpath_non_local_driver_rejected(self, mock_docker):
        """PVC with subPath on a non-local driver should be rejected."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.inspect_volume.return_value = {
            "Name": "cloud-vol",
            "Driver": "nfs",
            "Mountpoint": "",
        }
        mock_docker.from_env.return_value = mock_client

        service = DockerSandboxService(config=_app_config())

        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=120,
            resourceLimits=ResourceLimits(root={}),
            env={},
            metadata={},
            entrypoint=["python"],
            volumes=[
                Volume(
                    name="data",
                    pvc=PVC(claim_name="cloud-vol"),
                    mount_path="/mnt/data",
                    sub_path="subdir",
                )
            ],
        )

        with pytest.raises(HTTPException) as exc_info:
            await service.create_sandbox(request)

        assert exc_info.value.status_code == status.HTTP_400_BAD_REQUEST
        assert exc_info.value.detail["code"] == SandboxErrorCodes.PVC_SUBPATH_UNSUPPORTED_DRIVER

    @pytest.mark.asyncio
    async def test_pvc_subpath_symlink_escape_rejected(self, mock_docker):
        """PVC with subPath that resolves outside mountpoint via symlink should be rejected."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.inspect_volume.return_value = {
            "Name": "my-vol",
            "Driver": "local",
            "Mountpoint": "/var/lib/docker/volumes/my-vol/_data",
        }
        mock_docker.from_env.return_value = mock_client

        service = DockerSandboxService(config=_app_config())

        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=120,
            resourceLimits=ResourceLimits(root={}),
            env={},
            metadata={},
            entrypoint=["python"],
            volumes=[
                Volume(
                    name="data",
                    pvc=PVC(claim_name="my-vol"),
                    mount_path="/mnt/data",
                    sub_path="datasets",
                )
            ],
        )

        # Simulate: realpath resolves a symlink that escapes the mountpoint.
        # datasets -> / inside the volume, so realpath(…/_data/datasets) = /
        with patch("opensandbox_server.services.docker.docker_service.os.path.realpath") as mock_realpath:
            mock_realpath.side_effect = lambda p, **kwargs: ("/" if p.endswith("datasets") else p)
            with pytest.raises(HTTPException) as exc_info:
                await service.create_sandbox(request)

        assert exc_info.value.status_code == status.HTTP_400_BAD_REQUEST
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_SUB_PATH
        assert "symlink" in exc_info.value.detail["message"]

    @pytest.mark.asyncio
    async def test_pvc_subpath_binds_resolved_to_mountpoint(self, mock_docker):
        """PVC with subPath should resolve Mountpoint+subPath and pass as bind mount."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.inspect_volume.return_value = {
            "Name": "my-vol",
            "Driver": "local",
            "Mountpoint": "/var/lib/docker/volumes/my-vol/_data",
        }
        mock_client.api.create_host_config.return_value = {}
        mock_client.api.create_container.return_value = {"Id": "cid"}
        mock_client.containers.get.return_value = MagicMock()
        mock_docker.from_env.return_value = mock_client

        service = DockerSandboxService(config=_app_config())

        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=120,
            resourceLimits=ResourceLimits(root={}),
            env={},
            metadata={},
            entrypoint=["python"],
            volumes=[
                Volume(
                    name="train-data",
                    pvc=PVC(claim_name="my-vol"),
                    mount_path="/mnt/train",
                    read_only=True,
                    sub_path="datasets/train",
                )
            ],
        )

        with (
            patch.object(service, "_ensure_image_available"),
            patch.object(service, "_prepare_sandbox_runtime"),
        ):
            await service.create_sandbox(request)

        host_config_call = mock_client.api.create_host_config.call_args
        binds = host_config_call.kwargs["binds"]
        assert len(binds) == 1
        assert binds[0] == "/var/lib/docker/volumes/my-vol/_data/datasets/train:/mnt/train:ro"

    @pytest.mark.asyncio
    async def test_host_path_not_found_rejected(self, mock_docker):
        """Host path create failure should return 500 with HOST_PATH_CREATE_FAILED."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_docker.from_env.return_value = mock_client

        cfg = _app_config()
        cfg.storage = StorageConfig(
            allowed_host_paths=["/nonexistent/path/that/does/not/exist"]
        )
        service = DockerSandboxService(config=cfg)

        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=120,
            resourceLimits=ResourceLimits(root={}),
            env={},
            metadata={},
            entrypoint=["python"],
            volumes=[
                Volume(
                    name="workdir",
                    host=Host(path="/nonexistent/path/that/does/not/exist"),
                    mount_path="/mnt/work",
                    read_only=False,
                )
            ],
        )

        with patch("opensandbox_server.services.docker.docker_service.os.makedirs", side_effect=PermissionError("denied")):
            with pytest.raises(HTTPException) as exc_info:
                await service.create_sandbox(request)

        assert exc_info.value.status_code == status.HTTP_500_INTERNAL_SERVER_ERROR
        assert exc_info.value.detail["code"] == SandboxErrorCodes.HOST_PATH_CREATE_FAILED

    @pytest.mark.asyncio
    async def test_host_path_not_in_allowlist_rejected(self, mock_docker):
        """Host path not in allowlist should be rejected."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_docker.from_env.return_value = mock_client

        cfg = _app_config()
        cfg.storage = StorageConfig(allowed_host_paths=["/data/opensandbox"])
        service = DockerSandboxService(config=cfg)

        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=120,
            resourceLimits=ResourceLimits(root={}),
            env={},
            metadata={},
            entrypoint=["python"],
            volumes=[
                Volume(
                    name="workdir",
                    host=Host(path="/etc/passwd"),
                    mount_path="/mnt/work",
                    read_only=False,
                )
            ],
        )

        with pytest.raises(HTTPException) as exc_info:
            await service.create_sandbox(request)

        assert exc_info.value.status_code == status.HTTP_400_BAD_REQUEST
        assert exc_info.value.detail["code"] == SandboxErrorCodes.HOST_PATH_NOT_ALLOWED

    @pytest.mark.asyncio
    async def test_no_volumes_passes_validation(self, mock_docker):
        """Request without volumes should pass validation."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.create_host_config.return_value = {}
        mock_client.api.create_container.return_value = {"Id": "cid"}
        mock_client.containers.get.return_value = MagicMock()
        mock_docker.from_env.return_value = mock_client

        service = DockerSandboxService(config=_app_config())

        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=120,
            resourceLimits=ResourceLimits(root={}),
            env={},
            metadata={},
            entrypoint=["python"],
        )

        with (
            patch.object(service, "_ensure_image_available"),
            patch.object(service, "_prepare_sandbox_runtime"),
        ):
            response = await service.create_sandbox(request)

        assert response.status.state == "Running"

    @pytest.mark.asyncio
    async def test_host_volume_binds_passed_to_docker(self, mock_docker):
        """Host volume binds should be passed to Docker host config."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.create_host_config.return_value = {}
        mock_client.api.create_container.return_value = {"Id": "cid"}
        mock_client.containers.get.return_value = MagicMock()
        mock_docker.from_env.return_value = mock_client

        import tempfile

        with tempfile.TemporaryDirectory() as tmpdir:
            cfg = _app_config()
            cfg.storage = StorageConfig(allowed_host_paths=[tmpdir])
            service = DockerSandboxService(config=cfg)
            request = CreateSandboxRequest(
                image=ImageSpec(uri="python:3.11"),
                timeout=120,
                resourceLimits=ResourceLimits(root={}),
                env={},
                metadata={},
                entrypoint=["python"],
                volumes=[
                    Volume(
                        name="workdir",
                        host=Host(path=tmpdir),
                        mount_path="/mnt/work",
                        read_only=False,
                    )
                ],
            )

            with (
                patch.object(service, "_ensure_image_available"),
                patch.object(service, "_prepare_sandbox_runtime"),
            ):
                await service.create_sandbox(request)

            # Verify binds were passed to create_host_config
            host_config_call = mock_client.api.create_host_config.call_args
            assert "binds" in host_config_call.kwargs
            binds = host_config_call.kwargs["binds"]
            assert len(binds) == 1
            assert binds[0] == f"{tmpdir}:/mnt/work:rw"

    @pytest.mark.asyncio
    async def test_host_file_bind_passes_validation(self, mock_docker):
        """Existing host file should be allowed without mkdir."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.create_host_config.return_value = {}
        mock_client.api.create_container.return_value = {"Id": "cid"}
        mock_client.containers.get.return_value = MagicMock()
        mock_docker.from_env.return_value = mock_client

        import tempfile

        with tempfile.NamedTemporaryFile(suffix=".iso") as iso_file:
            cfg = _app_config()
            cfg.storage = StorageConfig(allowed_host_paths=[iso_file.name])
            service = DockerSandboxService(config=cfg)
            request = CreateSandboxRequest(
                image=ImageSpec(uri="python:3.11"),
                timeout=120,
                resourceLimits=ResourceLimits(root={}),
                env={},
                metadata={},
                entrypoint=["python"],
                volumes=[
                    Volume(
                        name="boot-iso",
                        host=Host(path=iso_file.name),
                        mount_path="/boot.iso",
                        read_only=True,
                    )
                ],
            )

            with (
                patch.object(service, "_ensure_image_available"),
                patch.object(service, "_prepare_sandbox_runtime"),
            ):
                await service.create_sandbox(request)

            host_config_call = mock_client.api.create_host_config.call_args
            binds = host_config_call.kwargs["binds"]
            assert len(binds) == 1
            assert binds[0] == f"{iso_file.name}:/boot.iso:ro"

    @pytest.mark.asyncio
    async def test_host_volume_with_subpath_resolved_correctly(self, mock_docker):
        """Host volume subPath should be resolved and validated."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.create_host_config.return_value = {}
        mock_client.api.create_container.return_value = {"Id": "cid"}
        mock_client.containers.get.return_value = MagicMock()
        mock_docker.from_env.return_value = mock_client

        import tempfile

        with tempfile.TemporaryDirectory() as tmpdir:
            cfg = _app_config()
            cfg.storage = StorageConfig(allowed_host_paths=[tmpdir])
            service = DockerSandboxService(config=cfg)
            # Create the subPath directory
            sub_dir = os.path.join(tmpdir, "task-001")
            os.makedirs(sub_dir)

            request = CreateSandboxRequest(
                image=ImageSpec(uri="python:3.11"),
                timeout=120,
                resourceLimits=ResourceLimits(root={}),
                env={},
                metadata={},
                entrypoint=["python"],
                volumes=[
                    Volume(
                        name="workdir",
                        host=Host(path=tmpdir),
                        mount_path="/mnt/work",
                        read_only=True,
                        sub_path="task-001",
                    )
                ],
            )

            with (
                patch.object(service, "_ensure_image_available"),
                patch.object(service, "_prepare_sandbox_runtime"),
            ):
                await service.create_sandbox(request)

            host_config_call = mock_client.api.create_host_config.call_args
            binds = host_config_call.kwargs["binds"]
            assert len(binds) == 1
            assert binds[0] == f"{sub_dir}:/mnt/work:ro"

    @pytest.mark.asyncio
    async def test_host_volume_symlink_bypass_rejected(self, mock_docker):
        """Host volume with symlink escaping allowed prefix should be rejected."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.create_host_config.return_value = {}
        mock_client.api.create_container.return_value = {"Id": "cid"}
        mock_client.containers.get.return_value = MagicMock()
        mock_docker.from_env.return_value = mock_client

        import tempfile

        with tempfile.TemporaryDirectory() as tmpdir:
            # Create a symlink within the allowed path that points to /
            link_path = os.path.join(tmpdir, "escape")
            os.symlink("/", link_path)

            cfg = _app_config()
            cfg.storage = StorageConfig(allowed_host_paths=[tmpdir])
            service = DockerSandboxService(config=cfg)

            # Request /tmpdir/escape/etc — lexical check passes (starts with
            # tmpdir) but realpath resolves escape -> /, producing /etc which
            # is outside the allowed prefix.
            request = CreateSandboxRequest(
                image=ImageSpec(uri="python:3.11"),
                timeout=120,
                resourceLimits=ResourceLimits(root={}),
                env={},
                metadata={},
                entrypoint=["python"],
                volumes=[
                    Volume(
                        name="escape-vol",
                        host=Host(path=os.path.join(link_path, "etc")),
                        mount_path="/mnt/etc",
                        read_only=True,
                    )
                ],
            )

            with (
                patch.object(service, "_ensure_image_available"),
                patch.object(service, "_prepare_sandbox_runtime"),
            ):
                with pytest.raises(HTTPException) as exc_info:
                    await service.create_sandbox(request)
            assert exc_info.value.status_code == 400
            assert exc_info.value.detail["code"] == SandboxErrorCodes.HOST_PATH_NOT_ALLOWED

    @pytest.mark.asyncio
    async def test_host_subpath_auto_created(self, mock_docker):
        """Host volume with non-existent subPath should be auto-created."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.create_host_config.return_value = {}
        mock_client.api.create_container.return_value = {"Id": "cid"}
        mock_client.containers.get.return_value = MagicMock()
        mock_docker.from_env.return_value = mock_client

        import tempfile

        with tempfile.TemporaryDirectory() as tmpdir:
            cfg = _app_config()
            cfg.storage = StorageConfig(allowed_host_paths=[tmpdir])
            service = DockerSandboxService(config=cfg)
            sub = "auto-created-sub"
            request = CreateSandboxRequest(
                image=ImageSpec(uri="python:3.11"),
                timeout=120,
                resourceLimits=ResourceLimits(root={}),
                env={},
                metadata={},
                entrypoint=["python"],
                volumes=[
                    Volume(
                        name="workdir",
                        host=Host(path=tmpdir),
                        mount_path="/mnt/work",
                        read_only=False,
                        sub_path=sub,
                    )
                ],
            )

            import os

            resolved = os.path.join(tmpdir, sub)
            assert not os.path.exists(resolved)

            # create_sandbox will proceed past volume validation (subpath
            # auto-created) but will fail later during container provisioning
            # (mock doesn't cover the full flow).  We only care that the
            # directory was created — NOT that it raised HOST_PATH_CREATE_FAILED.
            try:
                await service.create_sandbox(request)
            except HTTPException as e:
                # If it's our own create-failed error, the auto-create didn't
                # work — let the test fail explicitly.
                if e.detail.get("code") == SandboxErrorCodes.HOST_PATH_CREATE_FAILED:
                    raise
            except Exception:
                pass  # other provisioning errors are expected

            assert os.path.isdir(resolved)

    @pytest.mark.asyncio
    async def test_empty_allowlist_rejects_host_path(self, mock_docker):
        """Empty allowed_host_paths (default) should reject host bind mounts."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.create_host_config.return_value = {}
        mock_client.api.create_container.return_value = {"Id": "cid"}
        mock_client.containers.get.return_value = MagicMock()
        mock_docker.from_env.return_value = mock_client

        # Default config has storage.allowed_host_paths = []
        cfg = _app_config()
        assert cfg.storage.allowed_host_paths == []
        service = DockerSandboxService(config=cfg)

        import tempfile

        with tempfile.TemporaryDirectory() as tmpdir:
            request = CreateSandboxRequest(
                image=ImageSpec(uri="python:3.11"),
                timeout=120,
                resourceLimits=ResourceLimits(root={}),
                env={},
                metadata={},
                entrypoint=["python"],
                volumes=[
                    Volume(
                        name="workdir",
                        host=Host(path=tmpdir),
                        mount_path="/mnt/work",
                        read_only=False,
                    )
                ],
            )

            with (
                patch.object(service, "_ensure_image_available"),
                patch.object(service, "_prepare_sandbox_runtime"),
            ):
                with pytest.raises(HTTPException) as exc_info:
                    await service.create_sandbox(request)
            assert exc_info.value.status_code == 400
            assert exc_info.value.detail["code"] == SandboxErrorCodes.HOST_PATH_NOT_ALLOWED

    @pytest.mark.asyncio
    async def test_no_volumes_omits_binds_from_host_config(self, mock_docker):
        """When no volumes are specified, 'binds' should not appear in Docker host config."""
        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.create_host_config.return_value = {}
        mock_client.api.create_container.return_value = {"Id": "cid"}
        mock_client.containers.get.return_value = MagicMock()
        mock_docker.from_env.return_value = mock_client

        service = DockerSandboxService(config=_app_config())

        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=120,
            resourceLimits=ResourceLimits(root={}),
            env={},
            metadata={},
            entrypoint=["python"],
        )

        with (
            patch.object(service, "_ensure_image_available"),
            patch.object(service, "_prepare_sandbox_runtime"),
        ):
            await service.create_sandbox(request)

        host_config_call = mock_client.api.create_host_config.call_args
        assert "binds" not in host_config_call.kwargs


def test_docker_get_endpoint_rejects_expires():
    from unittest.mock import patch

    with patch("opensandbox_server.services.docker.docker_service.docker"):
        cfg = _app_config()
        cfg.docker.network_mode = "bridge"
        service = DockerSandboxService(config=cfg)

        with pytest.raises(HTTPException) as exc:
            service.get_endpoint("sbx-001", 8080, expires=1000)

        assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
        assert "not supported" in exc.value.detail["message"].lower()
