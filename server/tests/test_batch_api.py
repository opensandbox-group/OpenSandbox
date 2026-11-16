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

"""Exercise real routes, ownership checks and SDK dispatch; mock only Kubernetes I/O."""

from copy import deepcopy
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock

import pytest
import yaml
from fastapi import HTTPException
from kubernetes import client as k8s
from urllib3 import HTTPConnectionPool
from urllib3.exceptions import MaxRetryError

from opensandbox_server.api import batch
from opensandbox_server.api.batch_schema import BATCH_MANAGED_LABEL
from opensandbox_server.config import AppConfig, KubernetesRuntimeConfig
from opensandbox_server.main import app
from opensandbox_server.services.constants import SANDBOX_TENANT_LABEL
from opensandbox_server.services.k8s.batch_service import BatchService
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.tenants.context import set_current_tenant
from opensandbox_server.tenants.models import TenantEntry

PREFIX = "/v1/kubernetes"
LABELS = {BATCH_MANAGED_LABEL: "server", SANDBOX_TENANT_LABEL: ""}
JOB = {
    "apiVersion": "batch/v1",
    "kind": "Job",
    "metadata": {"name": "archive"},
    "spec": {
        "backoffLimit": 0,
        "template": {
            "spec": {
                "restartPolicy": "Never",
                "containers": [{"name": "worker", "image": "busybox:1.37"}],
            }
        },
    },
}
CRONJOB = {
    "apiVersion": "batch/v1",
    "kind": "CronJob",
    "metadata": {"name": "scheduled"},
    "spec": {
        "schedule": "* * * * *",
        "timeZone": "Asia/Shanghai",
        "concurrencyPolicy": "Forbid",
        "suspend": True,
        "jobTemplate": {"spec": JOB["spec"]},
    },
}


def metadata(name="archive", uid="job-uid", labels=None, owners=None):
    return k8s.V1ObjectMeta(
        name=name,
        namespace="workloads",
        uid=uid,
        resource_version="42",
        labels=dict(LABELS) if labels is None else labels,
        owner_references=owners,
    )


def owner(kind, uid):
    return k8s.V1OwnerReference(
        api_version="batch/v1", kind=kind, name="parent", uid=uid, controller=True
    )


@pytest.fixture
def backend(monkeypatch):
    monkeypatch.setattr(K8sClient, "_load_config", lambda self: None)
    transport = K8sClient(KubernetesRuntimeConfig(read_qps=10000, write_qps=10000))
    api = MagicMock(spec=k8s.BatchV1Api)
    core = MagicMock(spec=k8s.CoreV1Api)
    api.api_client = k8s.ApiClient()
    transport._batch_v1_api = api
    transport._core_v1_api = core
    job = k8s.V1Job(api_version="batch/v1", kind="Job", metadata=metadata())
    cronjob = k8s.V1CronJob(metadata=metadata("scheduled", "cron-uid"))
    api.create_namespaced_job.return_value = api.read_namespaced_job.return_value = job
    api.create_namespaced_cron_job.return_value = api.read_namespaced_cron_job.return_value = (
        cronjob
    )
    api.replace_namespaced_cron_job.return_value = cronjob
    api.list_namespaced_job.return_value = k8s.V1JobList(items=[job])
    api.list_namespaced_cron_job.return_value = k8s.V1CronJobList(items=[cronjob])
    service = BatchService(transport, "workloads")
    app.dependency_overrides[batch.get_batch_service] = lambda: service
    yield SimpleNamespace(api=api, core=core, service=service, transport=transport, job=job)
    app.dependency_overrides.pop(batch.get_batch_service, None)
    set_current_tenant(None)
    api.api_client.close()


@pytest.mark.parametrize("resource", ["jobs", "cronjobs"])
def test_authentication_and_disabled_runtime(client, auth_headers, resource):
    assert client.get(f"{PREFIX}/{resource}").status_code == 401
    response = client.get(f"{PREFIX}/{resource}", headers=auth_headers)
    assert response.status_code == 501


def test_feature_gate_and_unauthenticated_config(monkeypatch):
    config = AppConfig.model_validate({
        "runtime": {"type": "kubernetes", "execd_image": "unused"},
        "kubernetes": {"batch_api_enabled": False},
    })
    monkeypatch.setattr(batch, "get_config", lambda: config)
    with pytest.raises(HTTPException) as caught:
        batch.get_batch_service()
    assert caught.value.status_code == 501
    assert config.kubernetes is not None
    config.kubernetes.batch_api_enabled = True
    with pytest.raises(HTTPException) as caught:
        batch.get_batch_service()
    assert caught.value.status_code == 403


def test_create_job_preserves_native_spec_and_applies_ownership(client, auth_headers, backend):
    request = deepcopy(JOB)
    response = client.post(f"{PREFIX}/jobs", json=request, headers=auth_headers)
    assert response.status_code == 201
    assert response.json()["metadata"]["uid"] == "job-uid"
    sent = backend.api.create_namespaced_job.call_args.kwargs
    assert sent["namespace"] == "workloads"
    assert sent["field_validation"] == "Strict"
    assert sent["_request_timeout"] == 30
    assert sent["body"]["metadata"]["labels"] == LABELS
    assert sent["body"]["spec"] == JOB["spec"]
    assert request == JOB


@pytest.mark.parametrize(
    "field,value,expected",
    [
        ("namespace", "other", 403),
        ("labels", {BATCH_MANAGED_LABEL: "server"}, 422),
        ("labels", {SANDBOX_TENANT_LABEL: "other"}, 422),
        ("ownerReferences", [], 422),
        ("name", "../escape", 422),
    ],
)
def test_create_rejects_invalid_envelopes(client, auth_headers, backend, field, value, expected):
    request = deepcopy(JOB)
    request["metadata"][field] = value
    response = client.post(f"{PREFIX}/jobs", json=request, headers=auth_headers)
    assert response.status_code == expected
    if expected == 422:
        assert response.json()["detail"]
        assert all("input" not in error for error in response.json()["detail"])
    backend.api.create_namespaced_job.assert_not_called()


def test_cronjob_creation_and_replacement_stamp_child_labels(client, auth_headers, backend):
    assert client.post(f"{PREFIX}/cronjobs", json=CRONJOB, headers=auth_headers).status_code == 201
    sent = backend.api.create_namespaced_cron_job.call_args.kwargs["body"]
    assert sent["spec"]["jobTemplate"]["metadata"]["labels"] == LABELS
    request = deepcopy(CRONJOB)
    request["spec"].update(suspend=False, schedule="0 2 * * *")
    response = client.put(f"{PREFIX}/cronjobs/scheduled", json=request, headers=auth_headers)
    assert response.status_code == 200
    sent = backend.api.replace_namespaced_cron_job.call_args.kwargs
    assert sent["body"]["metadata"]["resourceVersion"] == "42"
    assert sent["body"]["metadata"]["uid"] == "cron-uid"
    assert sent["body"]["spec"]["suspend"] is False
    assert sent["body"]["spec"]["timeZone"] == "Asia/Shanghai"
    assert sent["field_validation"] == "Strict"
    assert (
        client.put(f"{PREFIX}/cronjobs/other", json=request, headers=auth_headers).status_code
        == 400
    )


@pytest.mark.parametrize(
    "template",
    [
        None,
        {"spec": []},
        {"spec": {}, "metadata": []},
        {"spec": {}, "metadata": {"namespace": "other"}},
        {"spec": {}, "metadata": {"labels": {SANDBOX_TENANT_LABEL: "other"}}},
    ],
)
def test_invalid_cronjob_templates(client, auth_headers, backend, template):
    request = deepcopy(CRONJOB)
    request["spec"]["jobTemplate"] = template
    assert client.post(f"{PREFIX}/cronjobs", json=request, headers=auth_headers).status_code == 422
    backend.api.create_namespaced_cron_job.assert_not_called()


def test_other_tenant_and_unmanaged_resources_are_hidden(client, auth_headers, backend):
    own = backend.job
    foreign = k8s.V1Job(metadata=metadata(labels={**LABELS, SANDBOX_TENANT_LABEL: "other"}))
    unmanaged = k8s.V1Job(metadata=metadata(labels={}))
    backend.api.list_namespaced_job.return_value = k8s.V1JobList(items=[own, foreign, unmanaged])
    response = client.get(f"{PREFIX}/jobs", headers=auth_headers)
    assert len(response.json()["items"]) == 1
    assert backend.api.list_namespaced_job.call_args.kwargs["label_selector"] == (
        "opensandbox.io/batch-managed-by=server,opensandbox.io/tenant="
    )
    for resource in (foreign, unmanaged):
        backend.api.read_namespaced_job.return_value = resource
        assert client.get(f"{PREFIX}/jobs/archive", headers=auth_headers).status_code == 404
        assert client.delete(f"{PREFIX}/jobs/archive", headers=auth_headers).status_code == 404
    backend.api.delete_namespaced_job.assert_not_called()


def test_tenant_context_selects_namespace_and_owner(backend):
    set_current_tenant(TenantEntry("team-a", "team-a-ns"))
    service = BatchService(backend.transport, "workloads")
    backend.api.read_namespaced_job.return_value = k8s.V1Job(
        metadata=metadata(labels={**LABELS, SANDBOX_TENANT_LABEL: "team-a"})
    )
    service.get("job", "archive")
    assert backend.api.read_namespaced_job.call_args.kwargs["namespace"] == "team-a-ns"


def test_cronjob_children_require_parent_uid(client, auth_headers, backend):
    child = k8s.V1Job(metadata=metadata(owners=[owner("CronJob", "cron-uid")]))
    stale = k8s.V1Job(metadata=metadata(owners=[owner("CronJob", "old-uid")]))
    backend.api.list_namespaced_job.return_value = k8s.V1JobList(items=[backend.job, child, stale])
    response = client.get(f"{PREFIX}/cronjobs/scheduled/jobs", headers=auth_headers)
    assert response.status_code == 200
    assert len(response.json()["items"]) == 1


@pytest.mark.parametrize("resource,method", [("jobs", "job"), ("cronjobs", "cron_job")])
def test_delete_is_cascading_and_uid_guarded(client, auth_headers, backend, resource, method):
    response = client.delete(f"{PREFIX}/{resource}/archive", headers=auth_headers)
    assert response.status_code == 202
    sent = getattr(backend.api, f"delete_namespaced_{method}").call_args.kwargs
    assert sent["body"]["propagationPolicy"] == "Background"
    assert sent["body"]["preconditions"]["uid"] == ("job-uid" if method == "job" else "cron-uid")


def test_pod_diagnostics_authorize_owner_and_bound_logs(client, auth_headers, backend):
    pod = k8s.V1Pod(metadata=metadata("archive-abcde", owners=[owner("Job", "job-uid")]))
    other = k8s.V1Pod(metadata=metadata("other", owners=[owner("Job", "different-uid")]))
    backend.core.list_namespaced_pod.return_value = k8s.V1PodList(items=[pod, other])
    response = client.get(f"{PREFIX}/jobs/archive/pods", headers=auth_headers)
    assert [p["metadata"]["name"] for p in response.json()["items"]] == ["archive-abcde"]
    backend.core.read_namespaced_pod.return_value = pod
    backend.core.read_namespaced_pod_log.return_value = "archived\n"
    path = f"{PREFIX}/jobs/archive/pods/archive-abcde/logs"
    response = client.get(
        path + "?container=worker&tailLines=10&limitBytes=1024", headers=auth_headers
    )
    assert response.text == "archived\n"
    assert backend.core.read_namespaced_pod_log.call_args.kwargs["limit_bytes"] == 1024
    assert client.get(path + "?tailLines=-1", headers=auth_headers).status_code == 422
    backend.core.read_namespaced_pod.return_value = other
    assert client.get(path, headers=auth_headers).status_code == 404


@pytest.mark.parametrize("status", [400, 403, 404, 409, 422, 429, 500])
def test_kubernetes_errors_keep_status_without_leaking_body(client, auth_headers, backend, status):
    backend.api.create_namespaced_job.side_effect = k8s.ApiException(
        status=status, reason="Rejected"
    )
    backend.api.create_namespaced_job.side_effect.body = '{"message":"secret-upload-url"}'
    response = client.post(f"{PREFIX}/jobs", json=JOB, headers=auth_headers)
    assert response.status_code == (503 if status == 500 else status)
    assert set(response.json()) == {"code", "message"}
    assert "secret-upload-url" not in response.text


def test_transport_errors_are_service_unavailable(client, auth_headers, backend):
    backend.api.read_namespaced_job.side_effect = MaxRetryError(
        HTTPConnectionPool("localhost"), "/apis/batch/v1"
    )
    assert client.get(f"{PREFIX}/jobs/archive", headers=auth_headers).status_code == 503


def test_batch_contract_matches_routes_and_request_schemas():
    source = Path(__file__).resolve().parents[2] / "specs" / "kubernetes-batch-api.yml"
    contract = yaml.safe_load(source.read_text())
    actual = app.openapi()
    for path, operations in contract["paths"].items():
        for method, operation in operations.items():
            if method == "parameters":
                continue
            route = actual["paths"]["/v1" + path][method]
            assert route["operationId"] == operation["operationId"]
            success = next(code for code in operation["responses"] if code.startswith("2"))
            assert success in route["responses"]
    for name in ("BatchMetadata", "JobManifest", "CronJobManifest"):
        declared = contract["components"]["schemas"][name]
        generated = actual["components"]["schemas"][name]
        assert set(declared["properties"]) == set(generated["properties"])
        assert set(declared["required"]) == set(generated["required"])
        assert declared["additionalProperties"] is generated["additionalProperties"] is False
