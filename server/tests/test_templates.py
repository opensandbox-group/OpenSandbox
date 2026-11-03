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

"""Tests for fsb template management: repository, service, routes, and
template-based sandbox creation."""

from copy import deepcopy
from datetime import datetime, timezone

import pytest
from fastapi import FastAPI, HTTPException
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
from opensandbox_server.repositories.templates.sqlite import SQLiteFastSandboxTemplateRepository
from opensandbox_server.services.templates.template_models import (
    FastSandboxTemplateListQuery,
    FastSandboxTemplatePhase,
    FastSandboxTemplateRecord,
)
from opensandbox_server.services.templates.template_service import FastSandboxTemplateService
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.services.k8s.informer import WorkloadInformer
from opensandbox_server.services.fast_sandbox.generated import fastpath_pb2 as pb2


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


def _converge(service, crs, namespace: str, name: str, event: str = "MODIFIED") -> None:
    """Deliver the CR store's current state through the watch reactor."""
    handler = service._on_template_event(namespace)
    if event == "DELETED":
        handler("DELETED", {"metadata": {"name": name}})
        return
    handler("MODIFIED", deepcopy(crs.crs[(namespace, name)]))


@pytest.fixture
def repo(tmp_path):
    repository = SQLiteFastSandboxTemplateRepository(tmp_path / "templates.db")
    yield repository
    repository.close()


@pytest.fixture
def crs():
    return _FakeTemplateCRs()


@pytest.fixture
def service(repo, crs, monkeypatch):
    monkeypatch.setattr(WorkloadInformer, "start", lambda self: None)
    with patch.object(K8sClient, "_load_config"):
        k8s = K8sClient(KubernetesRuntimeConfig())
    crs.install(k8s)
    svc = FastSandboxTemplateService(_config(), repository=repo, k8s_client=k8s)
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
        "tpl-1", "ns-1", phase=FastSandboxTemplatePhase.SUCCEEDED, manifest_ref="s3://b/m", message=None
    )
    assert repo.get("tpl-1", "ns-1").phase is FastSandboxTemplatePhase.SUCCEEDED
    assert repo.get("tpl-1", "ns-1").manifest_ref == "s3://b/m"

    result = repo.list(FastSandboxTemplateListQuery(namespace="ns-1"))
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
    result = repo.list(FastSandboxTemplateListQuery(namespace="ns-1", metadata={"env": "prod"}))
    assert result.total_items == 2
    result = repo.list(
        FastSandboxTemplateListQuery(namespace="ns-1", metadata={"env": "prod", "a": "b"})
    )
    assert result.total_items == 1
    dotted = repo.list(
        FastSandboxTemplateListQuery(namespace="ns-1", metadata={"app.example.com/team": "core"})
    )
    assert dotted.total_items == 1
    assert dotted.items[0].template_id == "tpl-3"


def service_record(template_id, namespace, metadata=None):
    return FastSandboxTemplateRecord(
        template_id=template_id,
        namespace=namespace,
        crd_name=template_id,
        spec={"image": "alpine:3.19", "publish": "s3://b/p", "format": "native"},
        metadata=metadata or {},
        phase=FastSandboxTemplatePhase.PENDING,
        created_at=datetime.now(timezone.utc),
        updated_at=datetime.now(timezone.utc),
    )


def test_create_projects_crd_with_server_side_inputs(service, crs):
    record = service.create_template(_create_request())
    assert record.phase is FastSandboxTemplatePhase.PENDING
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
    # CA wiring is opt-in: unconfigured servers create CA-free templates.
    assert "egressMitmCertificate" not in spec


@pytest.mark.parametrize("secret", ["sandbox-registry-credentials", "custom-registry"])
def test_create_oci_projects_registry_secret(service, crs, secret):
    service._k8s_config.template_oci_publish_secret = secret
    record = service.create_template(
        _create_request(publish="oci://registry.example.com/team/templates", format="overlaybd")
    )
    output = crs.created[-1]["spec"]["output"]
    assert output["publish"] == record.publish == "oci://registry.example.com/team/templates"
    assert output["format"] == "overlaybd"
    assert output["publishSecretRef"] == {"name": secret}


@pytest.mark.parametrize("publish", [
    "oci://registry.example.com/team/templates:latest",
    "oci://registry.example.com/team/templates@sha256:" + "a" * 64,
    "oci://registry.example.com/team/templates:",
    "oci://registry.example.com/team/templates@",
    "oci://registry.example.com/",
    "oci://registry.example.com",
    "oci://user:password@registry.example.com/team/templates",
    "oci://registry.example.com/UPPER",
    "oci://registry.example.com/team//templates",
    "oci://registry.example.com/team/templates?x=1",
    "oci://registry.example.com/team/templates#fragment",
])
def test_create_rejects_invalid_oci_destination_before_side_effects(service, crs, repo, publish):
    with pytest.raises(HTTPException) as excinfo:
        service.create_template(_create_request(publish=publish, format="overlaybd"))
    assert excinfo.value.status_code == 400
    assert excinfo.value.detail["code"] == "SANDBOX::INVALID_PARAMETER"
    assert not crs.created
    assert repo.list(FastSandboxTemplateListQuery(namespace="ns-1")).total_items == 0


@pytest.mark.parametrize("publish", [
    "oci://localhost:5000/team/templates",
    "oci://[::1]:5000/team/templates",
    "oci://registry.example.com/team/my__templates-v1",
])
def test_create_accepts_oci_repositories(overlaybd_service, crs, publish):
    overlaybd_service.create_template(_create_request(publish=publish, format="overlaybd"))
    assert crs.created[-1]["spec"]["output"]["publish"] == publish


def test_create_rejects_native_oci_before_side_effects(service, crs):
    with pytest.raises(HTTPException) as excinfo:
        service.create_template(_create_request(publish="oci://registry.example.com/team/templates"))
    assert excinfo.value.status_code == 400
    assert "S3-compatible" in excinfo.value.detail["message"]
    assert not crs.created


@pytest.mark.parametrize("manifest_ref", [
    None,
    "oci://registry.example.com/team/templates:latest",
    "oci://registry.example.com/team/templates@sha256:bad",
    "oci://registry.example.com/other/templates@sha256:" + "a" * 64,
    "s3://bucket/manifest.json",
])
def test_resolve_oci_requires_immutable_manifest_in_publish_repository(
    overlaybd_service, crs, manifest_ref
):
    service = overlaybd_service
    record = service.create_template(
        _create_request(publish="oci://registry.example.com/team/templates", format="overlaybd")
    )
    crs.set_status("ns-1", record.crd_name, {"phase": "Succeeded", "manifestRef": manifest_ref})
    _converge(service, crs, "ns-1", record.crd_name)
    with pytest.raises(HTTPException) as excinfo:
        service.resolve_template_artifact(record.template_id)
    assert excinfo.value.status_code == 404


def test_oci_builds_from_same_source_resolve_independently(overlaybd_service, crs):
    service = overlaybd_service
    records = [service.create_template(_create_request(
        publish="oci://registry.example.com/team/templates", format="overlaybd",
    )) for _ in range(2)]
    manifests = []
    for record, digit in zip(records, ["a", "b"]):
        manifest_ref = record.publish + "@sha256:" + digit * 64
        manifests.append(manifest_ref)
        crs.set_status("ns-1", record.crd_name, {"phase": "Succeeded", "manifestRef": manifest_ref})
        _converge(service, crs, "ns-1", record.crd_name)
    assert records[0].source_image == records[1].source_image
    assert [service.resolve_template_artifact(record.template_id)[0] for record in records] == manifests


def test_create_bakes_configured_mitm_ca(repo, crs, monkeypatch):
    monkeypatch.setattr(WorkloadInformer, "start", lambda self: None)
    with patch.object(K8sClient, "_load_config"):
        k8s = K8sClient(KubernetesRuntimeConfig())
    crs.install(k8s)
    config = _config()
    config.kubernetes = KubernetesRuntimeConfig(
        namespace="ns-1",
        template_s3_publish_secret="sandbox-oss-credentials",
        template_mitm_ca_secret="opensandbox-mitm-ca-v1",
    )
    svc = FastSandboxTemplateService(config, repository=repo, k8s_client=k8s)
    try:
        svc.create_template(_create_request())
        assert crs.created[-1]["spec"]["egressMitmCertificate"] == {
            "secretRef": {"name": "opensandbox-mitm-ca-v1"},
            "key": "ca.crt",
        }

        config.kubernetes.template_mitm_ca_secret_key = "tls.crt"
        svc.create_template(_create_request(image="alpine:3.20"))
        assert crs.created[-1]["spec"]["egressMitmCertificate"] == {
            "secretRef": {"name": "opensandbox-mitm-ca-v1"},
            "key": "tls.crt",
        }
    finally:
        svc.close()


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


def test_create_projects_env_to_crd(service, crs):
    service.create_template(_create_request(env={"LOG_LEVEL": "info", "API_KEY": "k"}))
    spec = crs.created[-1]["spec"]
    assert sorted((e["name"], e["value"]) for e in spec["envs"]) == [
        ("API_KEY", "k"),
        ("LOG_LEVEL", "info"),
    ]

    service.create_template(_create_request())
    assert "envs" not in crs.created[-1]["spec"]


@pytest.fixture
def execd_init_service(repo, crs, monkeypatch):
    monkeypatch.setattr(WorkloadInformer, "start", lambda self: None)
    with patch.object(K8sClient, "_load_config"):
        k8s = K8sClient(KubernetesRuntimeConfig())
    crs.install(k8s)
    config = _config()
    config.runtime.execd_run_as_init = True
    svc = FastSandboxTemplateService(config, repository=repo, k8s_client=k8s)
    yield svc
    svc.close()


def test_create_injects_execd_init_env_when_enabled(execd_init_service, crs):
    # A client-provided EXECD_INIT must not win over the server switch.
    record = execd_init_service.create_template(
        _create_request(env={"EXECD_INIT": "0", "LOG_LEVEL": "info"})
    )
    envs = {e["name"]: e["value"] for e in crs.created[-1]["spec"]["envs"]}
    assert envs == {"EXECD_INIT": "1", "LOG_LEVEL": "info"}
    # The stored row keeps the client env; injection is projection-only.
    assert record.spec["env"] == {"EXECD_INIT": "0", "LOG_LEVEL": "info"}

    execd_init_service.create_template(_create_request())
    assert crs.created[-1]["spec"]["envs"] == [{"name": "EXECD_INIT", "value": "1"}]


def test_create_does_not_inject_execd_init_by_default(service, crs):
    service.create_template(_create_request(env={"LOG_LEVEL": "info"}))
    envs = {e["name"]: e["value"] for e in crs.created[-1]["spec"]["envs"]}
    assert envs == {"LOG_LEVEL": "info"}

    service.create_template(_create_request())
    assert "envs" not in crs.created[-1]["spec"]


def test_create_rejects_invalid_env_names(service):
    from fastapi import HTTPException

    for name in ["1BAD", "HAS-DASH", "HAS.EQ=VAL", ""]:
        with pytest.raises(HTTPException) as excinfo:
            service.create_template(_create_request(env={name: "x"}))
        assert excinfo.value.status_code == 400
        assert "env names" in str(excinfo.value.detail).lower()


@pytest.mark.parametrize("whitespace", ["\n", "\r\n", "\t", " "])
def test_create_rejects_env_names_with_trailing_whitespace(service, crs, whitespace):
    from fastapi import HTTPException

    with pytest.raises(HTTPException) as excinfo:
        service.create_template(_create_request(env={"LOG_LEVEL" + whitespace: "x"}))
    assert excinfo.value.status_code == 400
    assert "env names" in str(excinfo.value.detail).lower()
    assert crs.created == []


def test_create_rolls_back_row_on_crd_conflict(service, crs, repo):
    crs.crs[("ns-1", "tpl-dup")] = {"metadata": {"name": "tpl-dup", "namespace": "ns-1"}}

    with patch(
        "opensandbox_server.services.templates.template_service.uuid.uuid4",
        return_value=type("U", (), {"__str__": lambda self: "dup"})(),
    ):
        with pytest.raises(Exception) as excinfo:
            service.create_template(_create_request())
    assert excinfo.value.status_code == 409


def test_get_reads_watch_converged_row(service, crs, repo):
    record = service.create_template(_create_request())
    crs.set_status(
        "ns-1",
        record.crd_name,
        {"phase": "Succeeded", "manifestRef": "s3://sandbox-images/publish/<build>/manifest.json"},
    )
    _converge(service, crs, "ns-1", record.crd_name)
    synced = service.get_template(record.template_id)
    assert synced.phase is FastSandboxTemplatePhase.SUCCEEDED
    assert synced.manifest_ref.startswith("s3://")
    assert repo.get(record.template_id, "ns-1").phase is FastSandboxTemplatePhase.SUCCEEDED


def test_get_marks_failed_when_crd_gone(service, crs, repo):
    record = service.create_template(_create_request())
    crs.crs.pop(("ns-1", record.crd_name))
    _converge(service, crs, "ns-1", record.crd_name, event="DELETED")
    synced = service.get_template(record.template_id)
    assert synced.phase is FastSandboxTemplatePhase.FAILED
    assert synced.manifest_ref is None


def test_failed_phase_carries_message(service, crs):
    record = service.create_template(_create_request())
    crs.set_status(
        "ns-1",
        record.crd_name,
        {"phase": "Failed", "conditions": [{"type": "Failed", "message": "kernel panic"}]},
    )
    _converge(service, crs, "ns-1", record.crd_name)
    synced = service.get_template(record.template_id)
    assert synced.phase is FastSandboxTemplatePhase.FAILED
    assert synced.message == "kernel panic"


def test_list_shows_watch_converged_phases(service, crs):
    first = service.create_template(_create_request(metadata={"env": "prod"}))
    second = service.create_template(_create_request(metadata={"env": "dev"}))
    crs.set_status("ns-1", first.crd_name, {"phase": "Succeeded", "manifestRef": "s3://b/m"})
    _converge(service, crs, "ns-1", first.crd_name)
    items, total = service.list_templates(metadata={"env": "prod"})
    assert total == 1
    assert items[0].template_id == first.template_id
    assert items[0].phase is FastSandboxTemplatePhase.SUCCEEDED
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
    assert persisted.phase is FastSandboxTemplatePhase.SUCCEEDED
    assert persisted.manifest_ref == "s3://b/m"


def test_watch_reactor_ignores_foreign_crds_and_deletes(service, crs, repo):
    record = service.create_template(_create_request())
    handler = service._on_template_event("ns-1")

    handler("MODIFIED", {"metadata": {"name": "tpl-orphan"}, "status": {"phase": "Succeeded"}})
    assert repo.get(record.template_id, "ns-1").phase is FastSandboxTemplatePhase.PENDING

    handler("DELETED", {"metadata": {"name": record.crd_name}})
    assert repo.get(record.template_id, "ns-1").phase is FastSandboxTemplatePhase.FAILED


def test_resolve_artifact_requires_succeeded(service, crs):
    record = service.create_template(_create_request(entrypoint=["sleep", "1"]))
    with pytest.raises(Exception) as excinfo:
        service.resolve_template_artifact(record.template_id)
    assert excinfo.value.status_code == 404

    crs.set_status("ns-1", record.crd_name, {"phase": "Succeeded", "manifestRef": "s3://b/m"})
    _converge(service, crs, "ns-1", record.crd_name)
    artifact_ref, entrypoint = service.resolve_template_artifact(record.template_id)
    assert artifact_ref == record.template_id
    assert entrypoint == ["sleep", "1"]


def test_resolve_gate_trusts_db_row_not_crd(service, crs):
    """The create gate reads the DB projection only."""
    record = service.create_template(_create_request(entrypoint=["sleep", "1"]))

    # CR already says Succeeded but the event has not been delivered: 404.
    crs.set_status("ns-1", record.crd_name, {"phase": "Succeeded", "manifestRef": "s3://b/m"})
    with pytest.raises(Exception) as excinfo:
        service.resolve_template_artifact(record.template_id)
    assert excinfo.value.status_code == 404

    # Event lands -> row converges -> gate passes, and it keeps passing even
    # if the CR store regresses afterwards (the gate never re-reads the CR).
    _converge(service, crs, "ns-1", record.crd_name)
    crs.set_status("ns-1", record.crd_name, {"phase": "Failed"})
    artifact_ref, entrypoint = service.resolve_template_artifact(record.template_id)
    assert artifact_ref == record.template_id
    assert entrypoint == ["sleep", "1"]


def test_resolve_restarts_watch_for_non_terminal_rows(service, crs, repo):
    """A non-terminal row re-arms its namespace watch on the create gate."""
    record = service_record("tpl-stuck", "ns-1")
    repo.create(record)
    assert "ns-1" not in service._watched_namespaces

    with pytest.raises(Exception) as excinfo:
        service.resolve_template_artifact(record.template_id)
    assert excinfo.value.status_code == 404
    assert "ns-1" in service._watched_namespaces


# -- overlaybd (OCI publication) ------------------------------------------------


def _overlaybd_config(**overrides) -> KubernetesRuntimeConfig:
    return KubernetesRuntimeConfig(
        namespace="ns-1",
        template_s3_publish_secret="sandbox-oss-credentials",
        template_oci_publish_secret="registry-credentials",
        **overrides,
    )


@pytest.fixture
def overlaybd_service(repo, crs, monkeypatch):
    monkeypatch.setattr(WorkloadInformer, "start", lambda self: None)
    with patch.object(K8sClient, "_load_config"):
        k8s = K8sClient(KubernetesRuntimeConfig())
    crs.install(k8s)
    config = _config()
    config.kubernetes = _overlaybd_config()
    svc = FastSandboxTemplateService(config, repository=repo, k8s_client=k8s)
    yield svc
    svc.close()


def _overlaybd_request(**overrides) -> CreateFsbTemplateRequest:
    overrides.setdefault("publish", "oci://registry.example.com/sandbox/templates")
    overrides.setdefault("format", "overlaybd")
    return _create_request(**overrides)


def test_create_overlaybd_projects_oci_output(overlaybd_service, crs):
    record = overlaybd_service.create_template(_overlaybd_request())
    spec = crs.created[-1]["spec"]
    assert spec["output"] == {
        "rootfsSize": "5Gi",
        "format": "overlaybd",
        "publish": "oci://registry.example.com/sandbox/templates",
        "publishSecretRef": {"name": "registry-credentials"},
    }
    assert record.spec["format"] == "overlaybd"


def test_resolve_overlaybd_returns_manifest_ref(overlaybd_service, crs):
    record = overlaybd_service.create_template(_overlaybd_request(entrypoint=["sleep", "1"]))
    manifest_ref = "oci://registry.example.com/sandbox/templates@sha256:" + "a" * 64
    crs.set_status("ns-1", record.crd_name, {"phase": "Succeeded", "manifestRef": manifest_ref})
    _converge(overlaybd_service, crs, "ns-1", record.crd_name)
    artifact_ref, entrypoint = overlaybd_service.resolve_template_artifact(record.template_id)
    assert artifact_ref == manifest_ref
    assert entrypoint == ["sleep", "1"]


def test_resolve_overlaybd_without_manifest_ref_404(overlaybd_service, crs):
    record = overlaybd_service.create_template(_overlaybd_request())
    crs.set_status("ns-1", record.crd_name, {"phase": "Succeeded"})
    _converge(overlaybd_service, crs, "ns-1", record.crd_name)
    with pytest.raises(HTTPException) as excinfo:
        overlaybd_service.resolve_template_artifact(record.template_id)
    assert excinfo.value.status_code == 404


def test_resolve_overlaybd_legacy_s3_manifest_demands_rebuild(overlaybd_service, crs):
    """Legacy overlaybd+S3 rows are unrunnable: resolve says so."""
    record = overlaybd_service.create_template(_overlaybd_request())
    crs.set_status(
        "ns-1", record.crd_name, {"phase": "Succeeded", "manifestRef": "s3://b/legacy/manifest"}
    )
    _converge(overlaybd_service, crs, "ns-1", record.crd_name)
    with pytest.raises(HTTPException) as excinfo:
        overlaybd_service.resolve_template_artifact(record.template_id)
    assert excinfo.value.status_code == 404
    assert "rebuild" in str(excinfo.value.detail).lower()


def test_create_overlaybd_requires_oci_publish_secret(repo, crs, monkeypatch):
    monkeypatch.setattr(WorkloadInformer, "start", lambda self: None)
    with patch.object(K8sClient, "_load_config"):
        k8s = K8sClient(KubernetesRuntimeConfig())
    crs.install(k8s)
    svc = FastSandboxTemplateService(_config(), repository=repo, k8s_client=k8s)
    try:
        with pytest.raises(HTTPException) as excinfo:
            svc.create_template(_overlaybd_request())
        assert excinfo.value.status_code == 400
        assert "template_oci_publish_secret" in str(excinfo.value.detail)
    finally:
        svc.close()


@pytest.mark.parametrize(
    "publish",
    [
        "s3://sandbox-images/publish",
        "registry.example.com/sandbox/templates",
        "oci://registry.example.com",
        "oci:///templates",
        "oci://registry.example.com/sandbox/templates:v1",
        "oci://registry.example.com/sandbox/templates@sha256:abc",
        "oci://registry.example.com/sandbox/templates/",
        "oci://user:pass@registry.example.com/sandbox/templates",
    ],
)
def test_create_overlaybd_rejects_invalid_publish(service, publish):
    with pytest.raises(HTTPException) as excinfo:
        service.create_template(_overlaybd_request(publish=publish))
    assert excinfo.value.status_code == 400


def test_create_overlaybd_allows_registry_host_port(overlaybd_service, crs):
    overlaybd_service.create_template(
        _overlaybd_request(publish="oci://registry.example.com:5000/sandbox/templates")
    )
    assert crs.created[-1]["spec"]["output"]["publish"] == (
        "oci://registry.example.com:5000/sandbox/templates"
    )


def test_create_native_rejects_oci_publish(service):
    with pytest.raises(HTTPException) as excinfo:
        service.create_template(
            _create_request(publish="oci://registry.example.com/sandbox/templates")
        )
    assert excinfo.value.status_code == 400
    assert "s3://" in str(excinfo.value.detail)


def test_create_native_rejects_bucketless_publish(service):
    with pytest.raises(HTTPException) as excinfo:
        service.create_template(_create_request(publish="s3://"))
    assert excinfo.value.status_code == 400
    assert "bucket" in str(excinfo.value.detail)


@pytest.fixture
def client(service, monkeypatch):
    app = FastAPI()
    app.include_router(templates_api.router, prefix="/v1")
    monkeypatch.setattr(templates_api, "_service", service)
    with TestClient(app) as test_client:
        yield test_client


@pytest.mark.parametrize("oci", [False, True])
def test_routes_template_lifecycle(client, crs, service, oci):
    if oci:
        service._k8s_config.template_oci_publish_secret = "registry-credentials"
    publish = "oci://registry.example.com/team/templates" if oci else "s3://sandbox-images/publish"
    manifest_ref = publish + "@sha256:" + "a" * 64 if oci else "s3://b/m"
    # Omit format for OCI to exercise the public API's overlaybd default.
    format_fields = {} if oci else {"format": "native"}
    response = client.post(
        "/v1/templates",
        json={
            "image": "alpine:3.19",
            "publish": publish,
            **format_fields,
            "env": {"LOG_LEVEL": "info"},
            "metadata": {"origin": "test"},
        },
    )
    assert response.status_code == 201, response.text
    body = response.json()
    assert body["status"]["phase"] == "Pending"
    assert body["publish"] == publish
    assert body["format"] == ("overlaybd" if oci else "native")
    assert body["env"] == {"LOG_LEVEL": "info"}
    template_id = body["templateId"]

    crs.set_status("ns-1", template_id, {"phase": "Succeeded", "manifestRef": manifest_ref})
    _converge(service, crs, "ns-1", template_id)

    detail = client.get(f"/v1/templates/{template_id}")
    assert detail.status_code == 200
    assert detail.json()["status"]["phase"] == "Succeeded"
    assert detail.json()["status"]["manifestRef"] == manifest_ref
    assert detail.json()["env"] == {"LOG_LEVEL": "info"}

    listing = client.get("/v1/templates", params={"metadata": "origin=test"})
    assert listing.status_code == 200
    payload = listing.json()
    assert payload["pagination"]["totalItems"] == 1
    assert payload["items"][0]["templateId"] == template_id

    deleted = client.delete(f"/v1/templates/{template_id}")
    assert deleted.status_code == 204
    assert client.get(f"/v1/templates/{template_id}").status_code == 404


def test_routes_list_filters_on_empty_metadata_value(client, service):
    empty = service.create_template(_create_request(metadata={"env": ""}))
    service.create_template(_create_request(metadata={"env": "prod"}))

    listing = client.get("/v1/templates", params={"metadata": "env="})
    assert listing.status_code == 200, listing.text
    payload = listing.json()
    assert payload["pagination"]["totalItems"] == 1
    assert [item["templateId"] for item in payload["items"]] == [empty.template_id]


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
        FastSandboxTemplateService(
            _config(runtime="kubernetes"),
            repository=SQLiteFastSandboxTemplateRepository(tmp_path / "templates.db"),
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


@pytest.mark.parametrize("oci", [False, True])
def test_template_mode_create_maps_artifact_and_entrypoint(service, crs, oci):
    import asyncio

    from opensandbox_server.services.fast_sandbox.service import FastSandboxService

    if oci:
        service._k8s_config.template_oci_publish_secret = "registry-credentials"
    publish = "oci://registry.example.com/team/templates" if oci else "s3://b/p"
    manifest_ref = publish + "@sha256:" + "a" * 64 if oci else "s3://b/m"
    record = service.create_template(_create_request(
        entrypoint=["python", "app.py"], publish=publish,
        format="overlaybd" if oci else "native",
    ))
    crs.set_status("ns-1", record.crd_name, {"phase": "Succeeded", "manifestRef": manifest_ref})
    _converge(service, crs, "ns-1", record.crd_name)

    with patch.object(K8sClient, "_load_config"):
        k8s = K8sClient(KubernetesRuntimeConfig())
    stub = _StubFastPath()
    sandbox_service = FastSandboxService(
        _config(), fastpath_client=stub, k8s_client=k8s, template_service=service
    )
    try:
        request = CreateSandboxRequest.model_validate(
            {"templateId": record.template_id, "timeout": 3600}
        )
        response = asyncio.run(sandbox_service.create_sandbox(request))
    finally:
        sandbox_service.close()

    assert stub.last_create.image == (manifest_ref if oci else record.template_id)
    assert list(stub.last_create.command) == ["python", "app.py"]
    assert stub.last_create.pool_ref == "default-pool"
    assert response.id.startswith("fsb-")
    assert response.entrypoint == ["python", "app.py"]


def test_template_mode_create_rejects_unknown_template(service):
    import asyncio

    from fastapi import HTTPException

    from opensandbox_server.services.fast_sandbox.service import FastSandboxService

    with patch.object(K8sClient, "_load_config"):
        k8s = K8sClient(KubernetesRuntimeConfig())
    sandbox_service = FastSandboxService(
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
    from opensandbox_server.services.fast_sandbox.service import FastSandboxService

    record = service.create_template(_create_request(entrypoint=["python", "app.py"]))
    crs.set_status("ns-1", record.crd_name, {"phase": "Succeeded", "manifestRef": "s3://b/m"})
    _converge(service, crs, "ns-1", record.crd_name)

    with patch.object(K8sClient, "_load_config"):
        k8s = K8sClient(KubernetesRuntimeConfig())
    fsb = FastSandboxService(
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
