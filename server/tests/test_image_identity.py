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

from datetime import datetime, timedelta, timezone
from types import SimpleNamespace
from unittest.mock import MagicMock

import pytest

from opensandbox_server.api.schema import ListSandboxesRequest, PaginationRequest, SandboxFilter
from opensandbox_server.services.composite_service import CompositeSandboxService
from opensandbox_server.services.image_identity import docker_image_digest, registry_image_digest
from opensandbox_server.services.k8s.image_identity import workload_image_digest
from opensandbox_server.services.k8s.kubernetes_service import KubernetesSandboxService
from opensandbox_server.services.k8s.workload_mapper import _build_sandbox_from_workload


DIGEST = "sha256:" + "a" * 64
OTHER_DIGEST = "sha256:" + "b" * 64


@pytest.mark.parametrize("prefix", ["", "docker-pullable://"])
def test_registry_identity_accepts_qualified_runtime_reference(prefix):
    assert registry_image_digest(prefix + "registry.example:5000/app@" + DIGEST) == DIGEST


@pytest.mark.parametrize(
    "reference",
    [
        None,
        DIGEST,
        "docker://" + DIGEST,
        "containerd://" + DIGEST,
        "containerd://example/app@" + DIGEST,
        "example/app:latest",
        "example/app@sha256:abc",
        "example/app@" + DIGEST.upper(),
        "example/app@" + DIGEST + "\n",
        "example/app@sha512:" + "a" * 128,
    ],
)
def test_registry_identity_rejects_config_ids_and_invalid_evidence(reference):
    assert registry_image_digest(reference) is None


def _container(references, requested="python:3.11"):
    return SimpleNamespace(
        attrs={"Config": {"Image": requested}},
        image=SimpleNamespace(attrs={"Id": OTHER_DIGEST, "RepoDigests": references}),
    )


@pytest.mark.parametrize("requested", ["python:3.11", "python@" + OTHER_DIGEST])
def test_docker_uses_attached_image_evidence_not_requested_tag_or_digest(requested):
    assert docker_image_digest(_container(["python@" + DIGEST], requested)) == DIGEST


def test_docker_repository_matching_handles_docker_hub_and_registry_ports():
    references = ["docker.io/library/python@" + DIGEST, "registry.example:5000/app@" + OTHER_DIGEST]
    assert docker_image_digest(_container(references)) == DIGEST
    assert (
        docker_image_digest(_container(references, "registry.example:5000/app:v1")) == OTHER_DIGEST
    )


@pytest.mark.parametrize(
    "references", [None, [], [DIGEST], ["python@" + DIGEST, "python@" + OTHER_DIGEST]]
)
def test_docker_omits_unavailable_or_ambiguous_registry_identity(references):
    assert docker_image_digest(_container(references)) is None


def test_docker_same_digest_across_repositories_is_unambiguous():
    assert (
        docker_image_digest(_container(["python@" + DIGEST, "mirror/python@" + DIGEST])) == DIGEST
    )


@pytest.mark.parametrize(
    "identity", [None, {}, {"Pull": []}, {"Pull": [{"Repository": "other/app"}]}]
)
def test_docker_containerd_local_descriptors_are_not_registry_pull_evidence(identity):
    container = _container(["python@" + DIGEST])
    container.image.attrs["Identity"] = identity
    assert docker_image_digest(container) is None


def test_docker_containerd_requires_matching_pull_repository():
    container = _container(["python@" + DIGEST])
    container.image.attrs["Identity"] = {"Pull": [{"Repository": "docker.io/library/python"}]}
    assert docker_image_digest(container) == DIGEST


def test_docker_containerd_descriptor_without_pull_identity_is_unknown():
    container = _container(["python@" + DIGEST])
    container.image.attrs["Descriptor"] = {"digest": DIGEST}
    assert docker_image_digest(container) is None


def _workload():
    return {
        "metadata": {
            "uid": "workload-uid",
            "namespace": "tenant",
            "labels": {"opensandbox.io/id": "sbx"},
            "creationTimestamp": "2026-10-09T00:00:00Z",
        },
        "spec": {
            "template": {"spec": {"containers": [{"name": "sandbox", "image": "python:3.11"}]}}
        },
        "status": {"selector": "opensandbox.io/id=sbx"},
    }


def _pod():
    return {
        "metadata": {
            "namespace": "tenant",
            "ownerReferences": [{"uid": "workload-uid", "controller": True}],
        },
        "status": {
            "containerStatuses": [
                {"name": "egress", "imageID": "example/egress@" + OTHER_DIGEST},
                {"name": "sandbox", "imageID": "docker-pullable://python@" + DIGEST},
            ],
            "initContainerStatuses": [
                {"name": "sandbox", "imageID": "example/init@" + OTHER_DIGEST}
            ],
        },
    }


def _provider(pods):
    provider = MagicMock()
    provider.k8s_client.list_pods.return_value = pods
    provider.get_expiration.return_value = None
    provider.get_status.return_value = {
        "state": "Running",
        "reason": "",
        "message": "",
        "last_transition_at": None,
    }
    return provider


def _batch_workload():
    workload = _workload()
    workload["apiVersion"] = "sandbox.opensandbox.io/v1alpha1"
    workload["kind"] = "BatchSandbox"
    workload["metadata"]["name"] = "sbx"
    workload["status"] = {"replicas": 1, "allocated": 1, "ready": 1}
    return workload


@pytest.mark.parametrize("stale_owner", [False, True])
def test_batchsandbox_without_status_selector_uses_controller_label_and_owner(stale_owner):
    pod = _pod()
    pod["metadata"]["labels"] = {"batch-sandbox.sandbox.opensandbox.io/name": "sbx"}
    if stale_owner:
        pod["metadata"]["ownerReferences"][0]["uid"] = "old-workload-uid"
    provider = _provider([pod])
    assert workload_image_digest(_batch_workload(), provider) == (None if stale_owner else DIGEST)
    provider.k8s_client.list_pods.assert_called_once_with(
        namespace="tenant", label_selector="batch-sandbox.sandbox.opensandbox.io/name=sbx"
    )


@pytest.mark.parametrize("name", [None, "", "sbx,other=value", "a" * 64])
def test_batchsandbox_invalid_label_value_does_not_query_pods(name):
    workload = _batch_workload()
    workload["metadata"]["name"] = name
    provider = _provider([_pod()])
    assert workload_image_digest(workload, provider) is None
    provider.k8s_client.list_pods.assert_not_called()


@pytest.mark.parametrize("composite", [False, True])
@pytest.mark.parametrize("page", [1, 2, 4])
@pytest.mark.parametrize("kind", ["BatchSandbox", "Sandbox"])
def test_list_reads_pods_only_for_filtered_page(composite, page, kind):
    provider = _provider([])
    workloads = []
    for index in range(61):
        workload = _batch_workload()
        workload["metadata"].update(
            name=f"sbx-{index}",
            uid=f"uid-{index}",
            creationTimestamp=(
                datetime(2026, 10, 9, tzinfo=timezone.utc) + timedelta(seconds=index)
            ).isoformat(),
            labels={
                "opensandbox.io/id": f"sbx-{index}",
                "team": "included" if index < 41 else "excluded",
            },
        )
        if kind == "Sandbox":
            workload["kind"] = kind
            workload["status"] = {"selector": f"opensandbox.io/id=sbx-{index}"}
        workloads.append(workload)
    provider.list_workloads.return_value = workloads

    def list_pods(*, namespace, label_selector):
        assert namespace == "tenant"
        index = int(label_selector.rsplit("-", 1)[1])
        pod = _pod()
        pod["metadata"]["ownerReferences"][0]["uid"] = f"uid-{index}"
        return [pod]

    provider.k8s_client.list_pods.side_effect = list_pods
    service = KubernetesSandboxService.__new__(KubernetesSandboxService)
    service.workload_provider = provider
    service._resolve_namespace = MagicMock(return_value="tenant")
    if composite:
        fsb = MagicMock()
        fsb_sandbox = _build_sandbox_from_workload(
            workloads[-1], provider, resolve_image_digest=False
        )
        fsb.list_sandbox_objects.return_value = [
            fsb_sandbox.model_copy(update={"id": "fsb-new", "metadata": {"team": "included"}})
        ]
        service = CompositeSandboxService(service, fsb)
    request = ListSandboxesRequest(
        filter=SandboxFilter(metadata={"team": "included"}),
        pagination=PaginationRequest(page=page, page_size=20),
    )
    response = service.list_sandboxes(request)
    assert response.pagination.total_items == (42 if composite else 41)
    all_ids = (["fsb-new"] if composite else []) + [f"sbx-{index}" for index in range(40, -1, -1)]
    expected_ids = all_ids[(page - 1) * 20:page * 20]
    expected_pod_ids = [sandbox_id for sandbox_id in expected_ids if not sandbox_id.startswith("fsb-")]
    assert [sandbox.id for sandbox in response.items] == expected_ids
    for sandbox in response.items:
        assert sandbox.resolved_image_digest == (None if sandbox.id.startswith("fsb-") else DIGEST)
    assert provider.k8s_client.list_pods.call_count == len(expected_pod_ids)
    selector_key = (
        "batch-sandbox.sandbox.opensandbox.io/name" if kind == "BatchSandbox" else "opensandbox.io/id"
    )
    assert [call.kwargs["label_selector"] for call in provider.k8s_client.list_pods.call_args_list] == [
        f"{selector_key}={sandbox_id}" for sandbox_id in expected_pod_ids
    ]
    provider.get_workload.assert_not_called()


def test_kubernetes_reads_owned_primary_container_and_maps_lifecycle_info():
    provider = _provider([_pod()])
    info = _build_sandbox_from_workload(_workload(), provider)
    assert info.resolved_image_digest == DIGEST
    assert info.model_dump(by_alias=True)["resolvedImageDigest"] == DIGEST
    provider.k8s_client.list_pods.assert_called_once_with(
        namespace="tenant", label_selector="opensandbox.io/id=sbx"
    )


def test_kubernetes_supports_native_client_objects():
    pod = SimpleNamespace(
        metadata=SimpleNamespace(
            namespace="tenant",
            owner_references=[SimpleNamespace(uid="workload-uid", controller=True)],
        ),
        status=SimpleNamespace(
            container_statuses=[SimpleNamespace(name="sandbox", image_id="python@" + DIGEST)]
        ),
    )
    assert workload_image_digest(_workload(), _provider([pod])) == DIGEST


@pytest.mark.parametrize(
    "case",
    [
        "missing",
        "wrong-owner",
        "non-controller",
        "wrong-namespace",
        "terminating",
        "multiple",
        "pending",
        "missing-main",
        "config-id",
        "duplicate-main",
    ],
)
def test_kubernetes_omits_digest_without_unambiguous_current_primary_container(case):
    pod = _pod()
    pods = [pod]
    if case == "missing":
        pods = []
    elif case == "wrong-owner":
        pod["metadata"]["ownerReferences"][0]["uid"] = "stale-workload"
    elif case == "non-controller":
        pod["metadata"]["ownerReferences"][0]["controller"] = False
    elif case == "wrong-namespace":
        pod["metadata"]["namespace"] = "other-tenant"
    elif case == "terminating":
        pod["metadata"]["deletionTimestamp"] = "2026-10-09T01:00:00Z"
    elif case == "multiple":
        pods.append(_pod())
    elif case == "pending":
        pod["status"]["containerStatuses"] = []
    elif case == "missing-main":
        pod["status"]["containerStatuses"] = pod["status"]["containerStatuses"][:1]
    elif case == "config-id":
        pod["status"]["containerStatuses"][1]["imageID"] = "docker://" + DIGEST
    elif case == "duplicate-main":
        pod["status"]["containerStatuses"].append(pod["status"]["containerStatuses"][1])
    assert workload_image_digest(_workload(), _provider(pods)) is None


@pytest.mark.parametrize("case", ["pool", "snapshot", "unbound", "deleting"])
def test_kubernetes_unsupported_workloads_do_not_query_pods(case):
    workload = _workload()
    if case == "pool":
        workload["spec"]["poolRef"] = "*"
    elif case == "snapshot":
        workload["metadata"]["labels"]["opensandbox.io/snapshot-id"] = "snap"
    elif case == "unbound":
        del workload["metadata"]["uid"]
    else:
        workload["metadata"]["deletionTimestamp"] = "2026-10-09T01:00:00Z"
    provider = _provider([_pod()])
    assert workload_image_digest(workload, provider) is None
    provider.k8s_client.list_pods.assert_not_called()


def test_kubernetes_optional_pod_lookup_failure_does_not_fail_lifecycle_read():
    provider = _provider([])
    provider.k8s_client.list_pods.side_effect = RuntimeError("RBAC denied")
    assert _build_sandbox_from_workload(_workload(), provider).resolved_image_digest is None
