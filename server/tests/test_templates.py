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

"""Tests for fsb template management: repository, service, routes, and
template-based sandbox creation."""

from copy import deepcopy
from datetime import datetime, timezone

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from kubernetes.client import ApiException, CustomObjectsApi, V1APIResource, V1APIResourceList
from unittest.mock import Mock, patch

from opensandbox_server.api import templates as templates_api
from opensandbox_server.api.schema import CreateFsbTemplateRequest, CreateSandboxRequest
from opensandbox_server.config import (
    AppConfig,
    KubernetesRuntimeConfig,
    RuntimeConfig,
    ServerConfig,
)
from opensandbox_server.repositories.templates.sqlite import SQLiteFsbTemplateRepository
from opensandbox_server.services.templates.template_models import (
    FsbTemplateListQuery,
    FsbTemplatePhase,
    FsbTemplateRecord,
)
from opensandbox_server.services.templates.template_service import FsbTemplateService
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.services.fsb.generated import fastpath_pb2 as pb2


def _config(namespace: str = "ns-1", runtime: str = "kubernetes") -> AppConfig:
    config = AppConfig(
        server=ServerConfig(host="0.0.0.0", port=8080, api_key="x"),
        runtime=RuntimeConfig(type=runtime, execd_image="opensandbox/execd:1.1.0"),
    )
    if runtime != "docker":
        config.kubernetes = KubernetesRuntimeConfig(
            namespace=namespace,
            template_s3_publish_secret="sandbox-oss-credentials",
        )
    return config


class _FakeTemplateCRs:
    """In-memory SandboxTemplate CR store behind a mocked CustomObjectsApi."""

    def __init__(self):
        self.crs: dict[tuple[str, str], dict] = {}
        self.created: list[dict] = []
        self.deleted: list[tuple[str, str]] = []

    def install(self, k8s: K8sClient) -> None:
        api = Mock(spec=CustomObjectsApi)
        api.get_api_resources.return_value = V1APIResourceList(
            group_version="sandbox.fast.io/v1alpha2",
            resources=[
                V1APIResource(
                    name="sandboxtemplates",
                    singular_name="sandboxtemplate",
                    kind="SandboxTemplate",
                    namespaced=True,
                    verbs=["get", "list", "watch"],
                )
            ],
        )

        def create_cr(**kwargs):
            body = kwargs["body"]
            key = (kwargs["namespace"], body["metadata"]["name"])
            if key in self.crs:
                raise ApiException(status=409)
            self.crs[key] = deepcopy(body)
            self.created.append(deepcopy(body))
            return deepcopy(body)

        def get_cr(**kwargs):
            cr = self.crs.get((kwargs["namespace"], kwargs["name"]))
            if cr is None:
                raise ApiException(status=404)
            return deepcopy(cr)

        def list_crs(**kwargs):
            return {
                "metadata": {"resourceVersion": "1"},
                "items": [
                    deepcopy(cr)
                    for (namespace, _), cr in self.crs.items()
                    if namespace == kwargs["namespace"]
                ],
            }

        def delete_cr(**kwargs):
            key = (kwargs["namespace"], kwargs["name"])
            if key not in self.crs:
                raise ApiException(status=404)
            self.deleted.append(key)
            self.crs.pop(key)

        api.create_namespaced_custom_object.side_effect = create_cr
        api.get_namespaced_custom_object.side_effect = get_cr
        api.list_namespaced_custom_object.side_effect = list_crs
        api.delete_namespaced_custom_object.side_effect = delete_cr
        k8s._custom_objects_api = api

    def set_status(self, namespace: str, name: str, status: dict) -> None:
        self.crs[(namespace, name)]["status"] = deepcopy(status)


@pytest.fixture
def repo(tmp_path):
    repository = SQLiteFsbTemplateRepository(tmp_path / "templates.db")
    yield repository
    repository.close()


@pytest.fixture
def crs():
    return _FakeTemplateCRs()


@pytest.fixture
def service(repo, crs):
    with patch.object(K8sClient, "_load_config"):
        k8s = K8sClient(KubernetesRuntimeConfig(informer_enabled=False))
    crs.install(k8s)
    svc = FsbTemplateService(_config(), repository=repo, k8s_client=k8s)
    yield svc
    svc.close()


def _create_request(**overrides) -> CreateFsbTemplateRequest:
    payload = {
        "image": "alpine:3.19",
        "publish": "s3://sandbox-images/publish",
        "format": "native",
        "resourceLimits": {"cpu": "2", "memory": "1Gi", "disk": "5Gi"},
        "entrypoint": ["python", "app.py"],
        "metadata": {"origin": "test"},
        "readiness": {"warmupSeconds": 15},
    }
    payload.update(overrides)
    return CreateFsbTemplateRequest.model_validate(payload)


def test_repository_roundtrip_and_tenant_scoping(repo):
    record = service_record("tpl-1", "ns-1")
    repo.create(record)
    repo.create(service_record("tpl-2", "ns-2"))

    assert repo.get("tpl-1", "ns-1").namespace == "ns-1"
    assert repo.get("tpl-1", "ns-2") is None
    assert repo.get("tpl-2", "ns-2").namespace == "ns-2"

    repo.update_status(
        "tpl-1", "ns-1", phase=FsbTemplatePhase.SUCCEEDED, manifest_ref="s3://b/m", message=None
    )
    assert repo.get("tpl-1", "ns-1").phase is FsbTemplatePhase.SUCCEEDED
    assert repo.get("tpl-1", "ns-1").manifest_ref == "s3://b/m"

    result = repo.list(FsbTemplateListQuery(namespace="ns-1"))
    assert result.total_items == 1

    repo.delete("tpl-1", "ns-1")
    assert repo.get("tpl-1", "ns-1") is None
    assert repo.get("tpl-2", "ns-2") is not None


def test_repository_metadata_filter(repo):
    for index, meta in enumerate(
        [
            {"env": "prod"},
            {"env": "dev"},
            {"env": "prod", "a": "b"},
            {"app.example.com/team": "core"},
        ]
    ):
        record = service_record(f"tpl-{index}", "ns-1", metadata=meta)
        repo.create(record)
    result = repo.list(FsbTemplateListQuery(namespace="ns-1", metadata={"env": "prod"}))
    assert result.total_items == 2
    result = repo.list(
        FsbTemplateListQuery(namespace="ns-1", metadata={"env": "prod", "a": "b"})
    )
    assert result.total_items == 1
    dotted = repo.list(
        FsbTemplateListQuery(namespace="ns-1", metadata={"app.example.com/team": "core"})
    )
    assert dotted.total_items == 1
    assert dotted.items[0].template_id == "tpl-3"


def service_record(template_id, namespace, metadata=None):
    return FsbTemplateRecord(
        template_id=template_id,
        namespace=namespace,
        crd_name=template_id,
        spec={"image": "alpine:3.19", "publish": "s3://b/p", "format": "native"},
        metadata=metadata or {},
        phase=FsbTemplatePhase.PENDING,
        created_at=datetime.now(timezone.utc),
        updated_at=datetime.now(timezone.utc),
    )


def test_create_projects_crd_with_server_side_inputs(service, crs):
    record = service.create_template(_create_request())
    assert record.phase is FsbTemplatePhase.PENDING
    assert len(crs.created) == 1
    crd = crs.created[0]
    assert crd["metadata"]["name"] == record.crd_name
    assert crd["metadata"]["namespace"] == "ns-1"
    assert crd["metadata"]["labels"] == {"origin": "test"}
    spec = crd["spec"]
    assert spec["image"] == "alpine:3.19"
    assert spec["indexKey"] == record.template_id
    assert spec["entrypoint"] == ["python", "app.py"]
    assert spec["execd"] == "opensandbox/execd:1.1.0"
    assert spec["kernel"] == "vmlinux.bin"
    assert spec["machine"] == {"vcpu": "2", "memory": "1Gi"}
    assert spec["readiness"] == {"warmupSeconds": 15}
    assert spec["output"]["rootfsSize"] == "5Gi"
    assert spec["output"]["format"] == "native"
    assert spec["output"]["publish"] == "s3://sandbox-images/publish"
    assert spec["output"]["publishSecretRef"] == {"name": "sandbox-oss-credentials"}


def test_create_defaults_machine_and_entrypoint(service, crs):
    service.create_template(_create_request(resourceLimits=None, entrypoint=None, readiness=None))
    spec = crs.created[-1]["spec"]
    assert spec["machine"] == {"vcpu": "1", "memory": "512Mi"}
    assert spec["output"]["rootfsSize"] == "2Gi"
    assert spec["entrypoint"] == ["tail", "-f", "/dev/null"]
    # The CRD requires the readiness object; an empty one lets structural
    # defaulting fill warmupSeconds=60.
    assert spec["readiness"] == {}


def test_create_maps_disk_resource_to_rootfs_size(service, crs):
    service.create_template(_create_request(resourceLimits={"disk": "10Gi"}))
    spec = crs.created[-1]["spec"]
    assert spec["output"]["rootfsSize"] == "10Gi"
    assert spec["machine"] == {"vcpu": "1", "memory": "512Mi"}


def test_create_rolls_back_row_on_crd_conflict(service, crs, repo):
    crs.crs[("ns-1", "tpl-dup")] = {"metadata": {"name": "tpl-dup", "namespace": "ns-1"}}

    with patch(
        "opensandbox_server.services.templates.template_service.uuid.uuid4",
        return_value=type("U", (), {"__str__": lambda self: "dup"})(),
    ):
        with pytest.raises(Exception) as excinfo:
            service.create_template(_create_request())
    assert excinfo.value.status_code == 409


def test_get_syncs_phase_from_crd(service, crs, repo):
    record = service.create_template(_create_request())
    crs.set_status(
        "ns-1",
        record.crd_name,
        {"phase": "Succeeded", "manifestRef": "s3://sandbox-images/publish/<build>/manifest.json"},
    )
    synced = service.get_template(record.template_id)
    assert synced.phase is FsbTemplatePhase.SUCCEEDED
    assert synced.manifest_ref.startswith("s3://")
    assert repo.get(record.template_id, "ns-1").phase is FsbTemplatePhase.SUCCEEDED


def test_get_marks_failed_when_crd_gone(service, crs, repo):
    record = service.create_template(_create_request())
    crs.crs.pop(("ns-1", record.crd_name))
    synced = service.get_template(record.template_id)
    assert synced.phase is FsbTemplatePhase.FAILED
    assert synced.manifest_ref is None


def test_failed_phase_carries_message(service, crs):
    record = service.create_template(_create_request())
    crs.set_status(
        "ns-1",
        record.crd_name,
        {"phase": "Failed", "conditions": [{"type": "Failed", "message": "kernel panic"}]},
    )
    synced = service.get_template(record.template_id)
    assert synced.phase is FsbTemplatePhase.FAILED
    assert synced.message == "kernel panic"


def test_list_bulk_syncs_and_filters(service, crs):
    first = service.create_template(_create_request(metadata={"env": "prod"}))
    second = service.create_template(_create_request(metadata={"env": "dev"}))
    crs.set_status("ns-1", first.crd_name, {"phase": "Succeeded", "manifestRef": "s3://b/m"})
    items, total = service.list_templates(metadata={"env": "prod"})
    assert total == 1
    assert items[0].template_id == first.template_id
    assert items[0].phase is FsbTemplatePhase.SUCCEEDED
    items_all, total_all = service.list_templates()
    assert total_all == 2
    assert {item.template_id for item in items_all} == {first.template_id, second.template_id}


def test_delete_removes_crd_and_row(service, crs, repo):
    record = service.create_template(_create_request())
    service.delete_template(record.template_id)
    assert repo.get(record.template_id, "ns-1") is None
    assert ("ns-1", record.crd_name) in crs.deleted

    second = service.create_template(_create_request())
    crs.crs.pop(("ns-1", second.crd_name))
    service.delete_template(second.template_id)
    assert repo.get(second.template_id, "ns-1") is None


def test_get_unknown_template_404(service):
    with pytest.raises(Exception) as excinfo:
        service.get_template("tpl-missing")
    assert excinfo.value.status_code == 404


def test_reserved_metadata_prefix_rejected(service):
    with pytest.raises(Exception) as excinfo:
        service.create_template(_create_request(metadata={"opensandbox.io/x": "y"}))
    assert excinfo.value.status_code == 400


def test_watch_reactor_converges_rows_without_reads(service, crs, repo):
    record = service.create_template(_create_request())

    handler = service._on_template_event("ns-1")
    crd = deepcopy(crs.crs[("ns-1", record.crd_name)])
    crd["status"] = {"phase": "Succeeded", "manifestRef": "s3://b/m"}
    handler("MODIFIED", crd)

    persisted = repo.get(record.template_id, "ns-1")
    assert persisted.phase is FsbTemplatePhase.SUCCEEDED
    assert persisted.manifest_ref == "s3://b/m"


def test_watch_reactor_ignores_foreign_crds_and_deletes(service, crs, repo):
    record = service.create_template(_create_request())
    handler = service._on_template_event("ns-1")

    handler("MODIFIED", {"metadata": {"name": "tpl-orphan"}, "status": {"phase": "Succeeded"}})
    assert repo.get(record.template_id, "ns-1").phase is FsbTemplatePhase.PENDING

    handler("DELETED", {"metadata": {"name": record.crd_name}})
    assert repo.get(record.template_id, "ns-1").phase is FsbTemplatePhase.FAILED


def test_resolve_artifact_requires_succeeded(service, crs):
    record = service.create_template(_create_request(entrypoint=["sleep", "1"]))
    with pytest.raises(Exception) as excinfo:
        service.resolve_template_artifact(record.template_id)
    assert excinfo.value.status_code == 404

    crs.set_status("ns-1", record.crd_name, {"phase": "Succeeded", "manifestRef": "s3://b/m"})
    artifact_ref, entrypoint = service.resolve_template_artifact(record.template_id)
    assert artifact_ref == record.template_id
    assert entrypoint == ["sleep", "1"]


@pytest.fixture
def client(service, monkeypatch):
    app = FastAPI()
    app.include_router(templates_api.router, prefix="/v1")
    monkeypatch.setattr(templates_api, "_service", service)
    with TestClient(app) as test_client:
        yield test_client


def test_routes_template_lifecycle(client, crs):
    response = client.post(
        "/v1/templates",
        json={
            "image": "alpine:3.19",
            "publish": "s3://sandbox-images/publish",
            "format": "native",
            "metadata": {"origin": "test"},
        },
    )
    assert response.status_code == 201, response.text
    body = response.json()
    assert body["status"]["phase"] == "Pending"
    template_id = body["templateId"]

    crs.set_status("ns-1", template_id, {"phase": "Succeeded", "manifestRef": "s3://b/m"})

    detail = client.get(f"/v1/templates/{template_id}")
    assert detail.status_code == 200
    assert detail.json()["status"]["phase"] == "Succeeded"
    assert detail.json()["status"]["manifestRef"] == "s3://b/m"

    listing = client.get("/v1/templates", params={"metadata": "origin%3Dtest"})
    assert listing.status_code == 200
    payload = listing.json()
    assert payload["pagination"]["totalItems"] == 1
    assert payload["items"][0]["templateId"] == template_id

    deleted = client.delete(f"/v1/templates/{template_id}")
    assert deleted.status_code == 204
    assert client.get(f"/v1/templates/{template_id}").status_code == 404


def test_routes_reject_extra_fields(client):
    response = client.post(
        "/v1/templates",
        json={"image": "alpine:3.19", "publish": "s3://b/p", "kernel": "vmlinux.bin"},
    )
    assert response.status_code == 422


def test_routes_501_for_non_kubernetes_runtime(monkeypatch, tmp_path):
    app = FastAPI()
    app.include_router(templates_api.router, prefix="/v1")
    monkeypatch.setattr(templates_api, "_service", None)
    monkeypatch.setattr(templates_api, "get_config", lambda: _config(runtime="docker"))
    with TestClient(app) as test_client:
        assert test_client.get("/v1/templates").status_code == 501
    monkeypatch.setattr(templates_api, "_service", None)
    monkeypatch.setattr(templates_api, "get_config", lambda: _config(runtime="kubernetes"))
    monkeypatch.setattr(
        templates_api,
        "_service",
        FsbTemplateService(
            _config(runtime="kubernetes"),
            repository=SQLiteFsbTemplateRepository(tmp_path / "templates.db"),
        ),
    )
    with TestClient(app) as test_client:
        assert test_client.get("/v1/templates").status_code == 200


def test_template_mode_request_validation():
    CreateSandboxRequest.model_validate({"templateId": "tpl-x", "timeout": 3600})

    for field, value in [
        ("entrypoint", ["x"]),
        ("env", {"A": "1"}),
        ("resourceLimits", {"cpu": "1"}),
        ("snapshotId", "snap-1"),
        ("secureAccess", True),
    ]:
        with pytest.raises(Exception):
            CreateSandboxRequest.model_validate(
                {"templateId": "tpl-x", "timeout": 3600, field: value}  # type: ignore[misc]
            )

    with pytest.raises(Exception):
        CreateSandboxRequest.model_validate({"templateId": "tpl-x"})

    with pytest.raises(Exception):
        CreateSandboxRequest.model_validate({"templateId": "tpl-x", "timeout": 10})


class _StubFastPath:
    def __init__(self):
        self.last_create = None
        info = pb2.SandboxInfo(
            identity=pb2.SandboxIdentity(uid="u-1", name="sbx", namespace="ns-1"),
            runtime=pb2.RuntimeInfo(state=pb2.RUNTIME_STATE_READY),
            data_plane=pb2.DataPlaneInfo(state=pb2.DATA_PLANE_STATE_READY),
        )
        self._response = pb2.CreateSandboxResponse(
            sandbox=info, generation=1, completion=pb2.CREATE_COMPLETION_READY
        )

    def create_sandbox(self, request, *, wait_timeout_millis=None):
        self.last_create = request
        return self._response

    def close(self):
        return None


def test_template_mode_create_maps_artifact_and_entrypoint(service, crs):
    import asyncio

    from opensandbox_server.services.fsb.service import FsbSandboxService

    record = service.create_template(_create_request(entrypoint=["python", "app.py"]))
    crs.set_status("ns-1", record.crd_name, {"phase": "Succeeded", "manifestRef": "s3://b/m"})

    with patch.object(K8sClient, "_load_config"):
        k8s = K8sClient(KubernetesRuntimeConfig(informer_enabled=False))
    stub = _StubFastPath()
    sandbox_service = FsbSandboxService(
        _config(), fastpath_client=stub, k8s_client=k8s, template_service=service
    )
    try:
        request = CreateSandboxRequest.model_validate(
            {"templateId": record.template_id, "timeout": 3600}
        )
        response = asyncio.run(sandbox_service.create_sandbox(request))
    finally:
        sandbox_service.close()

    assert stub.last_create.image == record.template_id
    assert list(stub.last_create.command) == ["python", "app.py"]
    assert stub.last_create.pool_ref == "default-pool"
    assert response.id.startswith("flt-")
    assert response.entrypoint == ["python", "app.py"]


def test_template_mode_create_rejects_unknown_template(service):
    import asyncio

    from fastapi import HTTPException

    from opensandbox_server.services.fsb.service import FsbSandboxService

    with patch.object(K8sClient, "_load_config"):
        k8s = K8sClient(KubernetesRuntimeConfig(informer_enabled=False))
    sandbox_service = FsbSandboxService(
        _config(), fastpath_client=_StubFastPath(), k8s_client=k8s, template_service=service
    )
    try:
        request = CreateSandboxRequest.model_validate({"templateId": "tpl-missing", "timeout": 3600})
        with pytest.raises(HTTPException) as excinfo:
            asyncio.run(sandbox_service.create_sandbox(request))
        assert excinfo.value.status_code == 404
    finally:
        sandbox_service.close()


def test_composite_routes_template_id_create_to_fsb(service, crs):
    import asyncio

    from unittest.mock import AsyncMock

    from opensandbox_server.api.schema import CreateSandboxResponse, SandboxStatus
    from opensandbox_server.services.composite_service import CompositeSandboxService
    from opensandbox_server.services.fsb.service import FsbSandboxService

    record = service.create_template(_create_request(entrypoint=["python", "app.py"]))
    crs.set_status("ns-1", record.crd_name, {"phase": "Succeeded", "manifestRef": "s3://b/m"})

    with patch.object(K8sClient, "_load_config"):
        k8s = K8sClient(KubernetesRuntimeConfig(informer_enabled=False))
    fsb = FsbSandboxService(
        _config(runtime="kubernetes"),
        fastpath_client=_StubFastPath(),
        k8s_client=k8s,
        template_service=service,
    )
    kubernetes = Mock()
    kubernetes.create_sandbox = AsyncMock(
        return_value=CreateSandboxResponse(
            id="sbx-k8s",
            status=SandboxStatus(state="Pending"),
            created_at=datetime.now(timezone.utc),
        )
    )
    composite = CompositeSandboxService(kubernetes, fsb)
    try:
        response = asyncio.run(
            composite.create_sandbox(
                CreateSandboxRequest.model_validate(
                    {"templateId": record.template_id, "timeout": 3600}
                )
            )
        )
        assert fsb._fastpath.last_create.image == record.template_id
        assert response.entrypoint == ["python", "app.py"]
        kubernetes.create_sandbox.assert_not_called()

        asyncio.run(
            composite.create_sandbox(
                CreateSandboxRequest.model_validate(
                    {"image": {"uri": "python:3.11"}, "entrypoint": ["sleep", "1"], "timeout": 600,
                     "resourceLimits": {"cpu": "1"}}
                )
            )
        )
        kubernetes.create_sandbox.assert_awaited_once()
    finally:
        composite.close()

def test_create_rejects_unsupported_resource_keys(service):
    with pytest.raises(Exception) as excinfo:
        service.create_template(_create_request(resourceLimits={"gpu": "1"}))
    assert excinfo.value.status_code == 400
    assert "gpu" in str(excinfo.value.detail["message"])
