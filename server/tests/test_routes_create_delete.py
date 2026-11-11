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

import json
import re
from datetime import datetime, timedelta, timezone
from unittest.mock import AsyncMock, MagicMock

import pytest
from docker.errors import NotFound as DockerNotFound
from fastapi.testclient import TestClient

from opensandbox_server.api import lifecycle
from opensandbox_server.api.schema import CreateSandboxRequest, CreateSandboxResponse, SandboxStatus
from opensandbox_server.config import AppConfig, DockerConfig, EgressConfig, RuntimeConfig
from opensandbox_server.services.constants import EGRESS_RULES_ENV
from opensandbox_server.services.docker import DockerSandboxService


def test_create_sandbox_openapi_describes_synchronous_provisioning(
    client: TestClient,
) -> None:
    response = client.get("/openapi.json")

    assert response.status_code == 200
    create_response = response.json()["paths"]["/v1/sandboxes"]["post"]["responses"]["202"]
    assert create_response["description"] == "Sandbox created and provisioned successfully"


def test_create_sandbox_openapi_declares_quota_rejection_403(
    client: TestClient,
) -> None:
    response = client.get("/openapi.json")

    assert response.status_code == 200
    responses = response.json()["paths"]["/v1/sandboxes"]["post"]["responses"]
    quota_response = responses["403"]
    assert quota_response["description"].startswith("Namespace ResourceQuota exhausted")
    assert quota_response["content"]["application/json"]["schema"]["$ref"].endswith("ErrorResponse")


@pytest.mark.parametrize(
    "network_policy",
    [
        None,
        {},
        {"egress": [{"action": "allow", "target": "example.com"}]},
        {"defaultAction": "allow", "egress": [{"action": "deny", "ports": [25]}]},
        {
            "defaultAction": "deny",
            "egress": [
                {"action": "allow", "target": "*.example.com"},
                {"action": "deny", "target": "10.0.0.0/8", "ports": [22, 443]},
            ],
        },
    ],
)
def test_create_sandbox_returns_202_and_service_payload(
    client: TestClient,
    auth_headers: dict,
    sample_sandbox_request: dict,
    monkeypatch,
    network_policy,
) -> None:
    now = datetime.now(timezone.utc)
    calls: list[CreateSandboxRequest] = []

    class StubService:
        @staticmethod
        async def create_sandbox(request) -> CreateSandboxResponse:
            calls.append(request)
            return CreateSandboxResponse(
                id="sbx-001",
                status=SandboxStatus(state="Pending"),
                metadata={"project": "test-project"},
                expiresAt=now + timedelta(hours=1),
                createdAt=now,
                entrypoint=["python", "-c", "print('Hello from sandbox')"],
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.post(
        "/v1/sandboxes",
        headers=auth_headers,
        json={**sample_sandbox_request, "networkPolicy": network_policy},
    )

    assert response.status_code == 202
    payload = response.json()
    assert payload["id"] == "sbx-001"
    assert payload["status"]["state"] == "Pending"
    assert payload["metadata"]["project"] == "test-project"
    assert payload["entrypoint"] == ["python", "-c", "print('Hello from sandbox')"]
    assert len(calls) == 1
    assert calls[0].image is not None
    assert calls[0].image.uri == "python:3.11"
    if network_policy is None:
        assert calls[0].network_policy is None
    else:
        assert calls[0].network_policy is not None
        assert calls[0].network_policy.model_dump(by_alias=True, exclude_none=True) == {
            "egress": [],
            **network_policy,
        }


def test_create_sandbox_manual_cleanup_omits_none_fields(
    client: TestClient,
    auth_headers: dict,
    sample_sandbox_request: dict,
    monkeypatch,
) -> None:
    now = datetime.now(timezone.utc)

    class StubService:
        @staticmethod
        async def create_sandbox(request) -> CreateSandboxResponse:
            return CreateSandboxResponse(
                id="sbx-manual",
                status=SandboxStatus(state="Pending"),
                metadata=None,
                expiresAt=None,
                createdAt=now,
                entrypoint=["python", "-c", "print('Hello from sandbox')"],
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())
    sample_sandbox_request.pop("timeout", None)

    response = client.post(
        "/v1/sandboxes",
        headers=auth_headers,
        json=sample_sandbox_request,
    )

    assert response.status_code == 202
    payload = response.json()
    assert "expiresAt" not in payload
    assert "metadata" not in payload
    assert "reason" not in payload["status"]
    assert "message" not in payload["status"]
    assert "lastTransitionAt" not in payload["status"]


def test_create_sandbox_rejects_invalid_request(
    client: TestClient,
    auth_headers: dict,
) -> None:
    response = client.post(
        "/v1/sandboxes",
        headers=auth_headers,
        json={"timeout": 10},
    )

    assert response.status_code == 422
    assert set(response.json()) == {"detail"}
    assert all(set(error) == {"type", "loc", "msg"} for error in response.json()["detail"])


@pytest.mark.parametrize(
    ("network_policy", "location", "error_type"),
    [
        ({"egress": [{"action": "allow", "target": "10.0.0.0/8", "port": [443]}]}, ["egress", 0, "port"], "extra_forbidden"),
        ({"egres": []}, ["egres"], "extra_forbidden"),
        ({"egress": [{"action": "allow", "target": "example.com", "ports": [443]}]}, ["egress", 0], "value_error"),
    ],
)
def test_create_sandbox_rejects_invalid_network_policy(
    client: TestClient,
    auth_headers: dict,
    sample_sandbox_request: dict,
    monkeypatch,
    network_policy,
    location,
    error_type,
) -> None:
    now = datetime.now(timezone.utc)
    create = AsyncMock(return_value=CreateSandboxResponse.model_validate({
        "id": "sbx-001",
        "status": {"state": "Pending"},
        "createdAt": now,
    }))
    monkeypatch.setattr(lifecycle.sandbox_service, "create_sandbox", create)

    response = client.post(
        "/v1/sandboxes",
        headers=auth_headers,
        json={**sample_sandbox_request, "networkPolicy": network_policy},
    )

    assert response.status_code == 422
    messages = {
        "extra_forbidden": "Extra inputs are not permitted",
        "value_error": "Value error, NetworkRule ports are not supported for domain targets yet.",
    }
    assert response.json() == {"detail": [{
        "type": error_type,
        "loc": ["body", "networkPolicy", *location],
        "msg": messages[error_type],
    }]}
    create.assert_not_called()


PVC_VOLUME = {"name": "data", "pvc": {"claimName": "data-vol"}, "mountPath": "/data"}
OSSFS_BACKEND = {
    "bucket": "my-bucket",
    "endpoint": "oss-cn-hangzhou.aliyuncs.com",
    "accessKeyId": "ak",
    "accessKeySecret": "sk",
}


def pending_sandbox() -> CreateSandboxResponse:
    return CreateSandboxResponse.model_validate({
        "id": "sbx-001",
        "status": {"state": "Pending"},
        "createdAt": datetime.now(timezone.utc),
    })


@pytest.mark.parametrize(
    ("patch", "location"),
    [
        ({"image": {"uri": "python:3.11", "auht": {"username": "u", "password": "p"}}}, ["image", "auht"]),
        (
            {"image": {"uri": "python:3.11", "auth": {"username": "u", "password": "p", "token": "t"}}},
            ["image", "auth", "token"],
        ),
        ({"platform": {"os": "linux", "arch": "arm64", "variant": "v8"}}, ["platform", "variant"]),
        ({"credentialProxy": {"enable": True}}, ["credentialProxy", "enable"]),
        ({"volumes": [{**PVC_VOLUME, "readonly": True}]}, ["volumes", 0, "readonly"]),
        (
            {"volumes": [{**PVC_VOLUME, "pvc": {"claimName": "data-vol", "createIfNotExist": False}}]},
            ["volumes", 0, "pvc", "createIfNotExist"],
        ),
        (
            {"volumes": [{"name": "data", "host": {"path": "/srv/data", "readonly": True}, "mountPath": "/data"}]},
            ["volumes", 0, "host", "readonly"],
        ),
        (
            {"volumes": [{"name": "data", "ossfs": {**OSSFS_BACKEND, "versoin": "1.0"}, "mountPath": "/data"}]},
            ["volumes", 0, "ossfs", "versoin"],
        ),
    ],
)
def test_create_sandbox_rejects_unknown_fields_in_closed_request_objects(
    client: TestClient,
    auth_headers: dict,
    sample_sandbox_request: dict,
    monkeypatch,
    patch,
    location,
) -> None:
    create = AsyncMock(return_value=pending_sandbox())
    monkeypatch.setattr(lifecycle.sandbox_service, "create_sandbox", create)

    response = client.post(
        "/v1/sandboxes",
        headers=auth_headers,
        json={**sample_sandbox_request, **patch},
    )

    assert response.status_code == 422
    assert response.json() == {"detail": [{
        "type": "extra_forbidden",
        "loc": ["body", *location],
        "msg": "Extra inputs are not permitted",
    }]}
    create.assert_not_called()


def snake_case_keys(value: dict) -> dict:
    """The same object with snake_case field names, which the models accept next to the camelCase aliases."""
    return {
        re.sub(r"(?<=[a-z])(?=[A-Z])", "_", key).lower(): snake_case_keys(item) if isinstance(item, dict) else item
        for key, item in value.items()
    }


def test_create_sandbox_accepts_declared_fields_aliases_and_open_maps(
    client: TestClient,
    auth_headers: dict,
    sample_sandbox_request: dict,
    monkeypatch,
) -> None:
    calls: list[CreateSandboxRequest] = []

    async def create_sandbox(request: CreateSandboxRequest) -> CreateSandboxResponse:
        calls.append(request)
        return pending_sandbox()

    monkeypatch.setattr(lifecycle.sandbox_service, "create_sandbox", create_sandbox)
    pvc = {
        "claimName": "data-vol",
        "createIfNotExists": False,
        "deleteOnSandboxTermination": True,
        "storageClass": "fast",
        "storage": "1Gi",
        "accessModes": ["ReadWriteOnce"],
    }
    volumes = [
        {"name": "pvc", "pvc": pvc, "mountPath": "/pvc", "readOnly": True, "subPath": "sub"},
        {"name": "host", "host": {"path": "/srv/data"}, "mountPath": "/host", "readOnly": False},
        {
            "name": "oss",
            "ossfs": {**OSSFS_BACKEND, "version": "1.0", "options": ["allow_other"]},
            "mountPath": "/oss",
            "readOnly": False,
        },
    ]
    snake_volumes = [
        {**volume, "name": f"{volume['name']}-snake", "mountPath": f"{volume['mountPath']}-snake"}
        for volume in volumes
    ]

    response = client.post(
        "/v1/sandboxes",
        headers=auth_headers,
        json={
            **sample_sandbox_request,
            "image": {"uri": "python:3.11", "auth": {"username": "u", "password": "p"}},
            "platform": {"os": "linux", "arch": "arm64"},
            "credentialProxy": {"enabled": True},
            "networkPolicy": {"defaultAction": "deny"},
            "metadata": {"any-key": "v", "example.com/owner": "a"},
            "env": {"ANY_VAR": "1", "UNSET_VAR": None},
            "extensions": {"vendor.example.com/flag": "on"},
            "resourceLimits": {"cpu": "500m", "memory": "512Mi", "vendor.example.com/gpu": "1"},
            "volumes": [*volumes, *(snake_case_keys(volume) for volume in snake_volumes)],
        },
    )

    assert response.status_code == 202
    parsed = calls[0]
    dumped = parsed.model_dump(by_alias=True, exclude_none=True)
    assert dumped["image"] == {"uri": "python:3.11", "auth": {"username": "u", "password": "p"}}
    assert dumped["platform"] == {"os": "linux", "arch": "arm64"}
    assert dumped["credentialProxy"] == {"enabled": True}
    assert dumped["resourceLimits"] == {"cpu": "500m", "memory": "512Mi", "vendor.example.com/gpu": "1"}
    assert dumped["volumes"] == [*volumes, *snake_volumes]
    assert parsed.metadata == {"any-key": "v", "example.com/owner": "a"}
    assert parsed.env == {"ANY_VAR": "1", "UNSET_VAR": None}
    assert parsed.extensions == {"vendor.example.com/flag": "on"}


@pytest.fixture
def docker_policy_runtime(monkeypatch):
    docker_client = MagicMock()
    docker_client.containers.list.return_value = []
    docker_client.api.create_container.side_effect = [
        {"Id": "sidecar-id"}, {"Id": "main-id"},
    ]
    docker_client.containers.get.side_effect = [
        MagicMock(id="sidecar-id"), MagicMock(id="main-id"),
    ]
    monkeypatch.setattr(
        "opensandbox_server.services.docker.docker_service.docker.from_env",
        lambda: docker_client,
    )
    service = DockerSandboxService(config=AppConfig(
        runtime=RuntimeConfig(type="docker", execd_image="execd:latest"),
        docker=DockerConfig(network_mode="bridge"),
        egress=EgressConfig(image="egress:latest"),
    ))
    monkeypatch.setattr(lifecycle, "sandbox_service", service)
    for name in (
        "_ensure_image_available",
        "_prepare_sandbox_runtime",
        "_wait_for_egress_sidecar_ready",
        "_schedule_expiration",
    ):
        monkeypatch.setattr(service, name, MagicMock())
    monkeypatch.setattr(
        "opensandbox_server.services.docker.docker_service.allocate_port_bindings",
        lambda *args, **kwargs: {
            "44772": ("0.0.0.0", 44772),
            "8080": ("0.0.0.0", 8080),
            "18080": ("0.0.0.0", 18080),
        },
    )
    return docker_client


@pytest.mark.parametrize(
    ("network_policy", "message"),
    [
        ({"egress": [{"action": "permit", "target": "example.com"}]}, "networkPolicy rule action must be allow or deny."),
        ({"defaultAction": "permit"}, "networkPolicy.defaultAction must be allow or deny."),
        ({"egress": [{"action": " DROP ", "target": "example.com"}]}, "networkPolicy rule action must be allow or deny."),
        ({"defaultAction": " BLOCK "}, "networkPolicy.defaultAction must be allow or deny."),
    ],
)
def test_create_docker_rejects_invalid_network_policy_actions(
    client, auth_headers, sample_sandbox_request, docker_policy_runtime, network_policy, message,
):
    response = client.post(
        "/v1/sandboxes",
        headers=auth_headers,
        json={**sample_sandbox_request, "metadata": {}, "networkPolicy": network_policy},
    )

    assert response.status_code == 400
    assert response.json() == {"code": "GENERAL::UNKNOWN_ERROR", "message": message}
    docker_policy_runtime.api.create_container.assert_not_called()


@pytest.mark.parametrize(
    ("network_policy", "expected"),
    [
        (
            {"defaultAction": "DENY", "egress": [{"action": " Allow ", "target": "example.com"}]},
            {"defaultAction": "deny", "egress": [{"action": "allow", "target": "example.com"}]},
        ),
        (
            {"defaultAction": " Allow ", "egress": [{"action": "DENY", "ports": [25]}]},
            {"defaultAction": "allow", "egress": [{"action": "deny", "ports": [25]}]},
        ),
        (
            {"defaultAction": "deny", "egress": [{"action": "allow", "target": "10.0.0.0/8", "ports": [443]}]},
            {"defaultAction": "deny", "egress": [{"action": "allow", "target": "10.0.0.0/8", "ports": [443]}]},
        ),
        ({"egress": [{"action": "allow", "target": "example.com"}]}, {"egress": [{"action": "allow", "target": "example.com"}]}),
        ({}, {"egress": []}),
    ],
)
def test_create_docker_sends_normalized_network_policy_to_sidecar(
    client, auth_headers, sample_sandbox_request, docker_policy_runtime, network_policy, expected,
):
    response = client.post(
        "/v1/sandboxes",
        headers=auth_headers,
        json={**sample_sandbox_request, "metadata": {}, "networkPolicy": network_policy},
    )

    assert response.status_code == 202
    sidecar_call = docker_policy_runtime.api.create_container.call_args_list[0]
    assert sidecar_call.kwargs["name"].startswith("sandbox-egress-")
    environment = dict(item.split("=", 1) for item in sidecar_call.kwargs["environment"])
    assert json.loads(environment[EGRESS_RULES_ENV]) == expected


@pytest.mark.parametrize(
    ("read_only", "status_code", "binds"),
    [
        ({"readOnly": True}, 202, ["data-vol:/data:ro"]),
        ({"read_only": True}, 202, ["data-vol:/data:ro"]),
        ({"readonly": True}, 422, None),
    ],
)
def test_create_docker_does_not_mount_a_misspelled_read_only_volume_read_write(
    client, auth_headers, sample_sandbox_request, docker_policy_runtime, read_only, status_code, binds,
):
    response = client.post(        "/v1/sandboxes",
        headers=auth_headers,
        json={**sample_sandbox_request, "metadata": {}, "volumes": [{**PVC_VOLUME, **read_only}]},
    )

    assert response.status_code == status_code
    if binds is None:
        docker_policy_runtime.api.create_container.assert_not_called()
    else:
        assert docker_policy_runtime.api.create_host_config.call_args.kwargs["binds"] == binds


@pytest.mark.parametrize(
    ("pvc", "status_code"),
    [
        ({"claimName": "data-vol", "createIfNotExists": False}, 400),
        ({"claimName": "data-vol", "createIfNotExist": False}, 422),
    ],
)
def test_create_docker_does_not_create_a_volume_when_the_create_flag_is_misspelled(
    client, auth_headers, sample_sandbox_request, docker_policy_runtime, pvc, status_code,
):
    docker_policy_runtime.api.inspect_volume.side_effect = [
        DockerNotFound("no such volume"),
        {"Name": "data-vol", "Driver": "local"},
    ]

    response = client.post(
        "/v1/sandboxes",
        headers=auth_headers,
        json={**sample_sandbox_request, "metadata": {}, "volumes": [{**PVC_VOLUME, "pvc": pvc}]},
    )

    assert response.status_code == status_code
    docker_policy_runtime.api.create_volume.assert_not_called()


def test_create_sandbox_accepts_snapshot_id_without_entrypoint(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    now = datetime.now(timezone.utc)
    calls: list[object] = []

    async def passthrough_resolve(request):
        return request

    monkeypatch.setattr(lifecycle, "resolve_sandbox_image_from_request", passthrough_resolve)

    class StubService:
        @staticmethod
        async def create_sandbox(request) -> CreateSandboxResponse:
            calls.append(request)
            return CreateSandboxResponse(
                id="sbx-from-snapshot",
                status=SandboxStatus(state="Pending"),
                metadata=None,
                expiresAt=now + timedelta(hours=1),
                createdAt=now,
                entrypoint=None,
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.post(
        "/v1/sandboxes",
        headers=auth_headers,
        json={
            "snapshotId": "snap-001",
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
        },
    )

    assert response.status_code == 202
    assert calls[0].snapshot_id == "snap-001"
    assert calls[0].entrypoint is None


def test_create_sandbox_accepts_snapshot_id_with_entrypoint(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    now = datetime.now(timezone.utc)
    calls: list[object] = []

    async def passthrough_resolve(request):
        return request

    monkeypatch.setattr(lifecycle, "resolve_sandbox_image_from_request", passthrough_resolve)

    class StubService:
        @staticmethod
        async def create_sandbox(request) -> CreateSandboxResponse:
            calls.append(request)
            return CreateSandboxResponse(
                id="sbx-from-snapshot",
                status=SandboxStatus(state="Pending"),
                metadata=None,
                expiresAt=now + timedelta(hours=1),
                createdAt=now,
                entrypoint=["python", "app.py"],
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.post(
        "/v1/sandboxes",
        headers=auth_headers,
        json={
            "snapshotId": "snap-001",
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
            "entrypoint": ["python", "app.py"],
        },
    )

    assert response.status_code == 202
    assert calls[0].snapshot_id == "snap-001"
    assert calls[0].entrypoint == ["python", "app.py"]


def test_delete_sandbox_returns_204_and_calls_service(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    calls: list[str] = []

    class StubService:
        @staticmethod
        def delete_sandbox(sandbox_id: str) -> None:
            calls.append(sandbox_id)

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.delete("/v1/sandboxes/sbx-001", headers=auth_headers)

    assert response.status_code == 204
    assert response.text == ""
    assert calls == ["sbx-001"]


def test_create_sandbox_pool_only_passes_resolution_untouched(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    now = datetime.now(timezone.utc)
    calls: list[CreateSandboxRequest] = []
    resolve_calls: list[CreateSandboxRequest] = []

    async def spy_resolve(request: CreateSandboxRequest):
        resolve_calls.append(request)
        return request

    monkeypatch.setattr(lifecycle, "resolve_sandbox_image_from_request", spy_resolve)

    class StubService:
        @staticmethod
        async def create_sandbox(request) -> CreateSandboxResponse:
            calls.append(request)
            return CreateSandboxResponse(
                id="sbx-pool",
                status=SandboxStatus(state="Pending"),
                metadata=None,
                expiresAt=now + timedelta(hours=1),
                createdAt=now,
                entrypoint=None,
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.post(
        "/v1/sandboxes",
        headers=auth_headers,
        json={
            "extensions": {"poolRef": "pool-a"},
            "timeout": 3600,
        },
    )

    # Pool-only creates carry no snapshot; resolution must pass them
    # through to the backend untouched (no 400/404 from the snapshot repo).
    assert response.status_code == 202, response.text
    assert len(resolve_calls) == 1
    assert calls[0].extensions == {"poolRef": "pool-a"}
