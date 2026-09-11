# pyright: reportAttributeAccessIssue=false
# protobuf-generated modules expose dynamic attributes.

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

"""HTTP-to-gRPC integration tests for the fsb runtime."""

from concurrent import futures
from copy import deepcopy
from datetime import datetime, timedelta, timezone
import json
from types import SimpleNamespace
from unittest.mock import Mock, patch

import grpc
import pytest
from fastapi import FastAPI, HTTPException
from fastapi.testclient import TestClient
from kubernetes.client import ApiException, CustomObjectsApi, V1APIResource, V1APIResourceList

from opensandbox_server.api import lifecycle, network_policy
from opensandbox_server.api.schema import RenewSandboxExpirationRequest
from opensandbox_server.config import (
    AppConfig,
    IngressConfig,
    KubernetesRuntimeConfig,
    RuntimeConfig,
    ServerConfig,
)
from opensandbox_server.services.fast_sandbox.fastpath_client import FastPathClient
from opensandbox_server.services.composite_service import CompositeSandboxService
from opensandbox_server.services.factory import create_sandbox_service
from opensandbox_server.services.fast_sandbox.service import FastSandboxService
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.services.k8s.informer import WorkloadInformer
from opensandbox_server.services.k8s.kubernetes_service import KubernetesSandboxService
from opensandbox_server.services.fast_sandbox.cr_mapping import METADATA_PREFIX
from opensandbox_server.services.fast_sandbox.generated import fastpath_pb2 as pb2
from opensandbox_server.services.fast_sandbox.generated import fastpath_pb2_grpc as pb2_grpc
from opensandbox_server.tenants.context import get_current_tenant, set_current_tenant
from opensandbox_server.tenants.models import TenantEntry


class _FakeFastPathService(pb2_grpc.FastPathServiceServicer):
    def __init__(self):
        self.sandboxes: dict[tuple[str, str], tuple[str, int]] = {}
        self.crs: dict[tuple[str, str], dict] = {}
        self.last_create: pb2.CreateSandboxRequest | None = None
        self.last_get: pb2.GetSandboxRequest | None = None
        self.last_update: pb2.UpdateSandboxRequest | None = None
        self.last_delete: pb2.DeleteRequest | None = None
        self.abort_update_with: grpc.StatusCode | None = None
        self.abort_create_with: grpc.StatusCode | None = None
        self.reject_create_with: grpc.StatusCode | None = None
        self.create_pending = False
        self.create_time_remaining: float | None = None
        self.diagnostic_runtime_state = pb2.RUNTIME_STATE_READY
        self.pool_has_resources = True
        self.get_error_by_namespace: dict[str, grpc.StatusCode] = {}

    @staticmethod
    def _info(name: str, uid: str, namespace: str = "ns-1") -> pb2.SandboxInfo:
        return pb2.SandboxInfo(
            identity=pb2.SandboxIdentity(uid=uid, name=name, namespace=namespace),
            applied_generation=1,
            runtime=pb2.RuntimeInfo(state=pb2.RUNTIME_STATE_READY),
            data_plane=pb2.DataPlaneInfo(state=pb2.DATA_PLANE_STATE_READY),
            ready=True,
        )

    def CreateSandbox(self, request, context):
        self.last_create = request
        if self.reject_create_with is not None:
            context.abort(self.reject_create_with, "scripted rejection before persistence")
        self.create_time_remaining = context.time_remaining()
        uid = f"uid-{request.request_id}"
        self.sandboxes[(request.namespace, request.request_id)] = (uid, 1)
        self.crs[(request.namespace, request.request_id)] = {
            "metadata": {
                "name": request.request_id,
                "namespace": request.namespace,
                "uid": uid,
                "generation": 1,
                "resourceVersion": "1",
                "creationTimestamp": datetime.now(timezone.utc).isoformat(),
                "labels": {METADATA_PREFIX + key: value for key, value in request.metadata.items()},
            },
            "spec": {
                "image": request.image,
                "command": list(request.command),
                "poolRef": request.pool_ref,
                "actionBindings": [
                    {"handler": b.handler, "input": b.input} for b in request.action_bindings
                ],
                "expireTime": datetime.fromtimestamp(
                    request.expires_at_unix_seconds, timezone.utc
                ).isoformat(),
            },
            "status": {
                "observedGeneration": 1,
                "runtime": {"state": "Creating" if self.create_pending else "Ready"},
                "dataPlane": {"state": "Pending" if self.create_pending else "Ready"},
                "conditions": [
                    {
                        "type": "Ready",
                        "status": "False" if self.create_pending else "True",
                        "observedGeneration": 1,
                    }
                ],
            },
        }
        if self.abort_create_with is not None:
            context.abort(self.abort_create_with, "scripted post-persistence failure")
        info = self._info(request.request_id, uid, request.namespace)
        if self.create_pending:
            info.runtime.state = pb2.RUNTIME_STATE_CREATING
            info.data_plane.state = pb2.DATA_PLANE_STATE_PENDING
            info.ready = False
        return pb2.CreateSandboxResponse(
            sandbox=info,
            generation=1,
            completion=request.completion,
        )

    def GetSandbox(self, request, context):
        self.last_get = request
        namespace = request.sandbox.namespaced_name.namespace
        name = request.sandbox.namespaced_name.name
        if error := self.get_error_by_namespace.get(namespace):
            context.abort(error, "scripted get failure")
        current = self.sandboxes.get((namespace, name))
        if current is None:
            return context.abort(grpc.StatusCode.NOT_FOUND, "not found")
        uid, generation = current
        if request.sandbox.expected_uid and request.sandbox.expected_uid != uid:
            context.abort(grpc.StatusCode.ABORTED, "uid fence rejected")
        if request.expected_generation and request.expected_generation != generation:
            context.abort(grpc.StatusCode.ABORTED, "generation fence rejected")
        return pb2.GetSandboxResponse(
            sandbox=self._info(name, uid, namespace), generation=generation
        )

    def UpdateSandbox(self, request, context):
        self.last_update = request
        if self.abort_update_with is not None:
            context.abort(self.abort_update_with, "scripted update failure")
        namespace = request.sandbox.namespaced_name.namespace
        name = request.sandbox.namespaced_name.name
        current = self.sandboxes.get((namespace, name))
        if current is None:
            return context.abort(grpc.StatusCode.NOT_FOUND, "not found")
        uid, generation = current
        if request.sandbox.expected_uid != uid or (
            request.expected_generation and request.expected_generation != generation
        ):
            context.abort(grpc.StatusCode.ABORTED, "fence rejected")
        self.sandboxes[(namespace, name)] = (uid, generation + 1)
        cr = self.crs[(namespace, name)]
        cr["metadata"]["generation"] = generation + 1
        cr["metadata"]["resourceVersion"] = str(generation + 1)
        if request.HasField("expires_at_unix_seconds"):
            cr["spec"]["expireTime"] = datetime.fromtimestamp(
                request.expires_at_unix_seconds, timezone.utc
            ).isoformat()
        if request.HasField("action_bindings"):
            cr["spec"]["actionBindings"] = [
                {"handler": b.handler, "input": b.input} for b in request.action_bindings.items
            ]
        for key, value in request.metadata_upsert.items():
            cr["metadata"]["labels"][METADATA_PREFIX + key] = value
        for key in request.metadata_delete_keys:
            cr["metadata"]["labels"].pop(METADATA_PREFIX + key, None)
        return pb2.UpdateSandboxResponse(
            sandbox=pb2.SandboxIdentity(uid=uid, name=name, namespace=namespace),
            committed_generation=generation + 1,
        )

    def DeleteSandbox(self, request, context):
        self.last_delete = request
        namespace = request.sandbox.namespaced_name.namespace
        name = request.sandbox.namespaced_name.name
        current = self.sandboxes.get((namespace, name))
        if current is None:
            return context.abort(grpc.StatusCode.NOT_FOUND, "not found")
        uid, _ = current
        if request.sandbox.expected_uid != uid:
            context.abort(grpc.StatusCode.ABORTED, "uid fence rejected")
        self.sandboxes.pop((namespace, name))
        self.crs.pop((namespace, name), None)
        return pb2.DeleteResponse()

    def GetSandboxDiagnostics(self, request, context):
        sandbox = self.sandboxes.get((request.namespace, request.sandbox_name))
        if sandbox is None:
            return context.abort(grpc.StatusCode.NOT_FOUND, "not found")
        uid, _ = sandbox
        info = self._info(request.sandbox_name, uid, request.namespace)
        info.runtime.state = self.diagnostic_runtime_state
        return pb2.SandboxDiagnosticsResponse(
            sandbox=info,
            assignment_state="assigned",
            events=[
                pb2.SandboxDiagnosticEvent(
                    timestamp_unix_nano=1,
                    level="Info",
                    source="fastlet",
                    phase="Ready",
                    message="sandbox ready",
                )
            ],
        )

    def GetPool(self, request, context):
        if not self.pool_has_resources:
            return pb2.PoolInfo(namespace=request.namespace, name=request.pool_name)
        return pb2.PoolInfo(
            namespace=request.namespace,
            name=request.pool_name,
            sandbox_cpu="500m",
            sandbox_memory="512Mi",
        )


@pytest.fixture
def http_fsb(monkeypatch):
    fake = _FakeFastPathService()
    grpc_server = grpc.server(futures.ThreadPoolExecutor(max_workers=4))
    pb2_grpc.add_FastPathServiceServicer_to_server(fake, grpc_server)
    port = grpc_server.add_insecure_port("127.0.0.1:0")
    grpc_server.start()

    config = AppConfig(
        server=ServerConfig(
            host="0.0.0.0",
            port=8080,
            api_key="x",
            max_sandbox_timeout_seconds=7200,
        ),
        runtime=RuntimeConfig(type="kubernetes", execd_image="ghcr.io/opensandbox/execd:latest"),
        kubernetes=KubernetesRuntimeConfig(namespace="ns-1"),
    )
    fastpath = FastPathClient(endpoint=f"127.0.0.1:{port}")
    with patch.object(K8sClient, "_load_config"):
        k8s = K8sClient(KubernetesRuntimeConfig(informer_enabled=False))
    api = Mock(spec=CustomObjectsApi)
    api.get_api_resources.return_value = V1APIResourceList(
        group_version="sandbox.fast.io/v1alpha2",
        resources=[
            V1APIResource(
                name="sandboxes",
                singular_name="sandbox",
                kind="Sandbox",
                namespaced=True,
                verbs=["get", "list", "watch"],
            )
        ],
    )

    def get_cr(**kwargs):
        cr = fake.crs.get((kwargs["namespace"], kwargs["name"]))
        if cr is None:
            raise ApiException(status=404)
        return deepcopy(cr)

    def list_crs(**kwargs):
        return {
            "metadata": {"resourceVersion": "1"},
            "items": [
                deepcopy(cr)
                for (namespace, _), cr in fake.crs.items()
                if namespace == kwargs["namespace"]
            ],
        }

    api.get_namespaced_custom_object.side_effect = get_cr
    api.list_namespaced_custom_object.side_effect = list_crs
    k8s._custom_objects_api = api
    service = FastSandboxService(config, fastpath_client=fastpath, k8s_client=k8s)
    monkeypatch.setattr(lifecycle, "sandbox_service", service)

    app = FastAPI()
    app.include_router(lifecycle.router, prefix="/v1")
    app.include_router(network_policy.router, prefix="/v1")
    try:
        with TestClient(app) as client:
            yield client, fake, service
    finally:
        service.close()
        grpc_server.stop(None)


def test_http_create_calls_fastpath_with_ready_completion(http_fsb):
    client, fake, service = http_fsb
    response = client.post(
        "/v1/sandboxes",
        json={
            "image": {"uri": "python:3.11"},
            "entrypoint": ["python", "-m", "http.server"],
            "timeout": 3600,
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
            "metadata": {"team": "agents"},
        },
    )

    assert response.status_code == 202
    body = response.json()
    assert body["id"].startswith("fsb-")
    assert body["status"]["state"] == "Running"
    assert body["metadata"] == {"team": "agents"}
    assert fake.last_create.request_id == body["id"]
    assert fake.last_create.completion == pb2.CREATE_COMPLETION_READY
    assert fake.create_time_remaining > service._k8s.fastpath_wait_ready_seconds


def test_http_create_recovers_an_ambiguous_post_persistence_failure(http_fsb):
    client, fake, _ = http_fsb
    fake.abort_create_with = grpc.StatusCode.INTERNAL

    response = client.post(
        "/v1/sandboxes",
        json={
            "image": {"uri": "python:3.11"},
            "entrypoint": ["python"],
            "timeout": 3600,
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
        },
    )

    assert response.status_code == 202
    sandbox_id = response.json()["id"]
    assert sandbox_id.startswith("fsb-")
    assert fake.last_get is not None
    assert ("ns-1", sandbox_id) in fake.sandboxes


def test_http_create_rejects_timeout_pool_mismatch_and_unreadable_extension(http_fsb):
    client, fake, _ = http_fsb
    base = {
        "image": {"uri": "python:3.11"},
        "entrypoint": ["python"],
        "timeout": 3600,
        "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
    }

    too_long = client.post("/v1/sandboxes", json={**base, "timeout": 10**9})
    pool_mismatch = client.post(
        "/v1/sandboxes",
        json={**base, "resourceLimits": {"cpu": "1", "memory": "512Mi"}},
    )
    renew_extension = client.post(
        "/v1/sandboxes",
        json={**base, "extensions": {"access.renew.extend.seconds": "300"}},
    )
    fake.pool_has_resources = False
    empty_pool_profile = client.post("/v1/sandboxes", json=base)

    assert too_long.status_code == 400
    assert pool_mismatch.status_code == 400
    assert "resourceLimits" in pool_mismatch.json()["detail"]["message"]
    assert renew_extension.status_code == 400
    assert "access.renew.extend.seconds" in renew_extension.json()["detail"]["message"]
    assert empty_pool_profile.status_code == 400
    assert (
        "not defined by the selected SandboxPool" in empty_pool_profile.json()["detail"]["message"]
    )


def test_http_read_and_metadata_patch_use_cr_fields(http_fsb):
    client, fake, _ = http_fsb
    created = client.post(
        "/v1/sandboxes",
        json={
            "image": {"uri": "python:3.11"},
            "entrypoint": ["python", "-m", "http.server"],
            "timeout": 3600,
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
            "metadata": {"team": "agents", "remove-me": "yes"},
        },
    ).json()
    sandbox_id = created["id"]

    responses = [
        client.get(f"/v1/sandboxes/{sandbox_id}"),
        client.patch(
            f"/v1/sandboxes/{sandbox_id}/metadata",
            json={"team": "platform", "remove-me": None},
        ),
        client.get("/v1/sandboxes", params={"pageSize": 10}),
    ]

    assert [response.status_code for response in responses] == [200, 200, 200]
    fetched, patched, listed = [response.json() for response in responses]
    cr = fake.crs[("ns-1", sandbox_id)]
    assert fetched["image"] == {"uri": "python:3.11"}
    assert fetched["entrypoint"] == ["python", "-m", "http.server"]
    assert datetime.fromisoformat(
        fetched["createdAt"].replace("Z", "+00:00")
    ) == datetime.fromisoformat(cr["metadata"]["creationTimestamp"])
    assert fetched["metadata"] == {"team": "agents", "remove-me": "yes"}
    assert patched["metadata"] == {"team": "platform"}
    assert fake.last_update.sandbox.expected_uid == cr["metadata"]["uid"]
    assert fake.last_update.expected_generation == 1
    assert listed["items"][0]["metadata"] == patched["metadata"]
    assert listed["pagination"]["totalItems"] == 1


@pytest.fixture
def persisted_fsb(http_fsb):
    client, fake, service = http_fsb
    response = client.post(
        "/v1/sandboxes",
        json={
            "image": {"uri": "python:3.11"},
            "entrypoint": ["python"],
            "timeout": 3600,
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
            "metadata": {"team": "agents"},
        },
    )
    assert response.status_code == 202
    sandbox_id = response.json()["id"]
    return client, fake, service, sandbox_id


def test_cr_reads_do_not_depend_on_fastpath_and_remain_tenant_scoped(persisted_fsb):
    client, fake, service, sandbox_id = persisted_fsb
    fake.get_error_by_namespace["ns-1"] = grpc.StatusCode.UNAVAILABLE
    other = deepcopy(fake.crs[("ns-1", sandbox_id)])
    other["metadata"]["namespace"] = "tenant-a"
    other["metadata"]["labels"][METADATA_PREFIX + "team"] = "other"
    fake.crs[("tenant-a", sandbox_id)] = other
    assert client.get(f"/v1/sandboxes/{sandbox_id}").status_code == 200
    previous = get_current_tenant()
    try:
        set_current_tenant(TenantEntry(name="tenant-a", namespace="tenant-a"))
        assert client.get(f"/v1/sandboxes/{sandbox_id}").json()["metadata"] == {"team": "other"}
        listed = client.get("/v1/sandboxes").json()
        assert listed["pagination"]["totalItems"] == 1
        assert listed["items"][0]["metadata"] == {"team": "other"}
        set_current_tenant(TenantEntry(name="empty", namespace="empty"))
        assert client.get(f"/v1/sandboxes/{sandbox_id}").status_code == 404
        assert client.get("/v1/sandboxes").json()["items"] == []
    finally:
        set_current_tenant(previous)


def test_cr_watch_and_fastpath_mutations_refresh_http_reads(persisted_fsb, monkeypatch):
    client, fake, service, sandbox_id = persisted_fsb
    k8s = service._cr_reader._client
    k8s.config.informer_enabled = True
    monkeypatch.setattr(WorkloadInformer, "start", lambda self: None)
    url = f"/v1/sandboxes/{sandbox_id}"
    assert client.get(url).json()["status"]["state"] == "Running"
    informer = k8s._lookup_informer("sandbox.fast.io", "v1alpha2", "sandboxes", "ns-1")
    assert informer._full_resync()

    cr = fake.crs[("ns-1", sandbox_id)]
    cr["status"]["dataPlane"]["state"] = "Pending"
    informer._handle_event({"type": "MODIFIED", "object": deepcopy(cr)})
    assert client.get(url).json()["status"]["state"] == "Pending"

    patched = client.patch(url + "/metadata", json={"team": "platform"})
    assert patched.status_code == 200
    assert patched.json()["metadata"] == {"team": "platform"}
    assert client.get("/v1/sandboxes").json()["items"][0]["metadata"] == {"team": "platform"}

    assert informer._full_resync()
    expires = (datetime.now(timezone.utc) + timedelta(hours=2)).replace(microsecond=0)
    assert (
        client.post(url + "/renew-expiration", json={"expiresAt": expires.isoformat()}).status_code
        == 200
    )
    assert (
        datetime.fromisoformat(client.get(url).json()["expiresAt"].replace("Z", "+00:00"))
        == expires
    )

    cr["status"]["runtime"]["state"] = "Stopped"
    assert informer._full_resync()
    assert client.get(url).json()["status"]["state"] == "Terminated"
    fake.crs.pop(("ns-1", sandbox_id))
    informer._handle_event({"type": "DELETED", "object": deepcopy(cr)})
    assert client.get(url).status_code == 404
    assert client.get("/v1/sandboxes").json()["items"] == []


def test_stale_ready_generation_is_not_running(persisted_fsb):
    client, fake, _, sandbox_id = persisted_fsb
    cr = fake.crs[("ns-1", sandbox_id)]
    cr["metadata"]["generation"] = 2
    assert client.get(f"/v1/sandboxes/{sandbox_id}").json()["status"]["state"] == "Pending"


def test_delete_uses_cr_identity_when_runtime_observation_is_gone(persisted_fsb):
    client, fake, _, sandbox_id = persisted_fsb
    uid = fake.crs[("ns-1", sandbox_id)]["metadata"]["uid"]
    fake.get_error_by_namespace["ns-1"] = grpc.StatusCode.UNAVAILABLE
    url = f"/v1/sandboxes/{sandbox_id}"
    assert client.delete(url).status_code == 204
    assert fake.last_delete.sandbox.expected_uid == uid
    assert client.delete(url).status_code == 404


def test_http_policy_replace_preserves_bindings_and_fences_updates(persisted_fsb):
    client, fake, _, sandbox_id = persisted_fsb
    url = f"/v1/sandboxes/{sandbox_id}/networkpolicy"
    assert client.get(url).json()["policy"] == {"defaultAction": "deny", "egress": []}
    bindings = [
        {"handler": "audit", "input": '{"enabled":true}'},
        {"handler": "egress", "input": " "},
        {"handler": "other", "input": "{}"},
    ]
    cr = fake.crs[("ns-1", sandbox_id)]
    cr["spec"]["actionBindings"] = bindings
    assert client.get(url).json()["mode"] == "deny_all"
    policy = {"defaultAction": "allow", "egress": []}
    response = client.put(url, json=policy)
    assert response.status_code == 200
    assert response.json()["policy"] == policy
    assert client.get(url).json()["policy"] == policy
    assert list(fake.last_update.action_bindings.items)[0].input == bindings[0]["input"]
    assert [b.handler for b in fake.last_update.action_bindings.items] == [
        "audit",
        "egress",
        "other",
    ]
    assert fake.last_update.action_bindings.items[2].input == bindings[2]["input"]
    assert json.loads(fake.last_update.action_bindings.items[1].input) == policy
    assert fake.last_update.sandbox.expected_uid == cr["metadata"]["uid"]
    assert fake.last_update.expected_generation == 1

    fake.abort_update_with = grpc.StatusCode.ABORTED
    assert client.put(url, json={"defaultAction": "deny"}).status_code == 409
    assert client.get(url).json()["policy"] == policy


def test_policy_rejects_invalid_input_and_other_tenant(persisted_fsb):
    client, fake, _, sandbox_id = persisted_fsb
    url = f"/v1/sandboxes/{sandbox_id}/networkpolicy"
    for policy in ({"defaultAction": "invalid"}, {"egress": [{"action": "allow", "target": " "}]}):
        assert client.put(url, json=policy).status_code == 400
    assert fake.last_update is None
    previous = get_current_tenant()
    try:
        set_current_tenant(TenantEntry(name="empty", namespace="empty"))
        assert client.get(url).status_code == 404
        assert client.put(url, json={"defaultAction": "allow"}).status_code == 404
        assert fake.last_update is None
    finally:
        set_current_tenant(previous)


def test_legacy_policy_route_preserves_body_for_existing_proxy(http_fsb, monkeypatch):
    from starlette.responses import JSONResponse

    client, _, _ = http_fsb

    async def proxy(request, sandbox_id, port, path):
        assert (sandbox_id, port, path) == ("legacy-id", 18080, "policy")
        body = b"".join([part async for part in request.stream()])
        return JSONResponse(json.loads(body) if body else {"status": "ok"})

    monkeypatch.setattr(network_policy, "_proxy_http_request", proxy)
    assert client.get("/v1/sandboxes/legacy-id/networkpolicy").json() == {"status": "ok"}
    policy = {"defaultAction": "allow", "egress": []}
    assert client.put("/v1/sandboxes/legacy-id/networkpolicy", json=policy).json() == policy


@pytest.mark.parametrize(
    "k8s_namespace,expected_namespace",
    [
        ("legacy", "legacy"),
        (None, "default"),
    ],
)
def test_kubernetes_configuration_automatically_composes_fsb(
    persisted_fsb, k8s_namespace, expected_namespace
):
    _, _, fsb, _ = persisted_fsb
    config = fsb._app_config.model_copy(
        update={
            "runtime": RuntimeConfig(type="kubernetes", execd_image="execd:test"),
            "kubernetes": KubernetesRuntimeConfig(namespace=k8s_namespace),
        }
    )
    with patch("opensandbox_server.services.factory.KubernetesSandboxService") as constructor:
        constructor.return_value.k8s_client = fsb._cr_reader._client
        service = create_sandbox_service(config=config)
    try:
        assert isinstance(service, CompositeSandboxService)
        assert service._fsb._resolve_namespace() == expected_namespace
        assert service._fsb._cr_reader._client is constructor.return_value.k8s_client
    finally:
        service.close()


def test_mixed_list_globally_filters_sorts_and_pages(persisted_fsb, monkeypatch):
    client, fake, fsb, sandbox_id = persisted_fsb
    base = fsb.get_sandbox(sandbox_id)
    cr = fake.crs[("ns-1", sandbox_id)]
    cr["metadata"]["creationTimestamp"] = "2026-01-02T00:00:00Z"
    legacy = Mock(spec=KubernetesSandboxService)
    legacy.list_sandbox_objects.return_value = [
        base.model_copy(
            update={"id": "legacy-new", "created_at": datetime(2026, 1, 3, tzinfo=timezone.utc)}
        ),
        base.model_copy(
            update={"id": "legacy-tie", "created_at": datetime(2026, 1, 2, tzinfo=timezone.utc)}
        ),
        base.model_copy(update={"id": "legacy-filtered", "metadata": {"team": "other"}}),
    ]
    monkeypatch.setattr(lifecycle, "sandbox_service", CompositeSandboxService(legacy, fsb))
    params = {"pageSize": 2, "metadata": "team=agents", "state": "Running"}
    first = client.get("/v1/sandboxes", params=params).json()
    second = client.get("/v1/sandboxes", params={**params, "page": 2}).json()
    assert [item["id"] for item in first["items"]] == ["legacy-new", sandbox_id]
    assert [item["id"] for item in second["items"]] == ["legacy-tie"]
    assert first["pagination"]["totalItems"] == second["pagination"]["totalItems"] == 3
    assert first["pagination"]["hasNextPage"] is True
    assert second["pagination"]["hasNextPage"] is False

    legacy.get_sandbox.return_value = base.model_copy(update={"id": "legacy-new"})
    assert client.get("/v1/sandboxes/legacy-new").status_code == 200
    assert client.get("/v1/sandboxes/fsb-missing").status_code == 404
    legacy.get_sandbox.assert_called_once_with("legacy-new")


@pytest.mark.parametrize(
    "failure,expected", [("absent", 200), ("forbidden", 503), ("list404", 503), ("legacy", 503)]
)
def test_mixed_list_never_hides_a_backend_failure(persisted_fsb, monkeypatch, failure, expected):
    client, _, fsb, sandbox_id = persisted_fsb
    legacy = Mock(spec=KubernetesSandboxService)
    legacy.list_sandbox_objects.return_value = [
        fsb.get_sandbox(sandbox_id).model_copy(update={"id": "legacy"})
    ]
    api = fsb._cr_reader._client.get_custom_objects_api()
    if failure in ("absent", "forbidden"):
        api.get_api_resources.side_effect = ApiException(status=404 if failure == "absent" else 403)
    elif failure == "list404":
        api.list_namespaced_custom_object.side_effect = ApiException(status=404)
    else:
        legacy.list_sandbox_objects.side_effect = ApiException(status=503)
    monkeypatch.setattr(lifecycle, "sandbox_service", CompositeSandboxService(legacy, fsb))
    response = client.get("/v1/sandboxes")
    assert response.status_code == expected
    if expected == 200:
        assert [item["id"] for item in response.json()["items"]] == ["legacy"]


@pytest.mark.asyncio
async def test_mixed_create_keeps_legacy_semantics(persisted_fsb):
    _, _, fsb, _ = persisted_fsb
    legacy = Mock(spec=KubernetesSandboxService)
    service = CompositeSandboxService(legacy, fsb)
    request = Mock(template_id=None)
    assert await service.create_sandbox(request) is legacy.create_sandbox.return_value
    legacy.create_sandbox.assert_awaited_once_with(request)


def test_http_renew_and_delete_use_uid_fences(http_fsb):
    client, fake, _ = http_fsb
    created = client.post(
        "/v1/sandboxes",
        json={
            "image": {"uri": "python:3.11"},
            "entrypoint": ["python"],
            "timeout": 3600,
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
        },
    ).json()
    sandbox_id = created["id"]
    expires_at = datetime.now(timezone.utc) + timedelta(hours=2)

    renewed = client.post(
        f"/v1/sandboxes/{sandbox_id}/renew-expiration",
        json={"expiresAt": expires_at.isoformat()},
    )
    past = client.post(
        f"/v1/sandboxes/{sandbox_id}/renew-expiration",
        json={"expiresAt": (datetime.now(timezone.utc) - timedelta(seconds=1)).isoformat()},
    )
    deleted = client.delete(f"/v1/sandboxes/{sandbox_id}")

    assert renewed.status_code == 200
    assert fake.last_update.sandbox.expected_uid == f"uid-{sandbox_id}"
    assert fake.last_update.expected_generation == 1
    assert past.status_code == 400
    assert deleted.status_code == 204
    assert fake.last_delete.sandbox.expected_uid == f"uid-{sandbox_id}"


def test_http_renew_maps_fence_conflict_and_missing_sandbox(http_fsb):
    client, fake, _ = http_fsb
    created = client.post(
        "/v1/sandboxes",
        json={
            "image": {"uri": "python:3.11"},
            "entrypoint": ["python"],
            "timeout": 3600,
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
        },
    ).json()
    expires_at = datetime.now(timezone.utc) + timedelta(hours=2)
    fake.abort_update_with = grpc.StatusCode.ABORTED

    conflict = client.post(
        f"/v1/sandboxes/{created['id']}/renew-expiration",
        json={"expiresAt": expires_at.isoformat()},
    )
    missing_renew = client.post(
        "/v1/sandboxes/fsb-missing/renew-expiration",
        json={"expiresAt": expires_at.isoformat()},
    )
    missing_delete = client.delete("/v1/sandboxes/fsb-missing")

    assert conflict.status_code == 409
    assert missing_renew.status_code == 404
    assert missing_delete.status_code == 404


def test_diagnostics_use_new_structured_state(http_fsb):
    client, fake, service = http_fsb
    sandbox_id = client.post(
        "/v1/sandboxes",
        json={
            "image": {"uri": "python:3.11"},
            "entrypoint": ["python"],
            "timeout": 3600,
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
        },
    ).json()["id"]
    content = service.get_sandbox_events(sandbox_id)

    assert '"runtime_state": "RUNTIME_STATE_READY"' in content
    assert '"data_plane_state": "DATA_PLANE_STATE_READY"' in content
    assert "sandbox ready" in content

    fake.diagnostic_runtime_state = 99
    assert '"runtime_state": "99"' in service.get_sandbox_events(sandbox_id)


def test_event_diagnostics_enforce_stable_scope_contract(http_fsb):
    _, _, service = http_fsb

    with pytest.raises(HTTPException) as exc_info:
        service.get_sandbox_event_diagnostics("fsb-1", "lifecycle")

    assert exc_info.value.status_code == 400
    assert exc_info.value.detail == {
        "code": "DIAGNOSTICS_SCOPE_UNSUPPORTED",
        "message": (
            "Unsupported events diagnostics scope 'lifecycle'. Supported scopes: runtime, all."
        ),
    }


@pytest.mark.parametrize("operation", ["logs", "pause", "resume"])
def test_unsupported_fsb_operations_are_explicit(http_fsb, operation):
    _, _, service = http_fsb

    with pytest.raises(HTTPException) as exc_info:
        if operation == "logs":
            service.get_sandbox_logs("fsb-1")
        elif operation == "pause":
            service.pause_sandbox("fsb-1")
        elif operation == "resume":
            service.resume_sandbox("fsb-1")

    assert exc_info.value.status_code == 501


@pytest.mark.parametrize("pending", [False, True])
def test_http_create_handles_capacity_rejection_and_accepted_pending(http_fsb, pending):
    client, fake, _ = http_fsb
    fake.create_pending = pending
    if not pending:
        fake.reject_create_with = grpc.StatusCode.RESOURCE_EXHAUSTED
    response = client.post(
        "/v1/sandboxes",
        json={
            "image": {"uri": "python:3.11"},
            "entrypoint": ["python"],
            "timeout": 60,
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
        },
    )
    if pending:
        assert response.status_code == 202
        assert response.json()["status"]["state"] == "Pending"
    else:
        assert response.status_code == 429
        assert response.headers["Retry-After"] == "1"


def _gateway_config(mode="header"):
    return IngressConfig.model_validate(
        {
            "mode": "gateway",
            "gateway": {
                "address": "*.example.com" if mode == "wildcard" else "ingress.example.com",
                "route": {"mode": mode},
            },
            "secure_access": {
                "active_key": "k",
                "keys": [{"key_id": "k", "key": "c2hhcmVkLXNlY3JldA=="}],
            },
        }
    )


@pytest.mark.parametrize("mode", ["header", "uri"])
def test_http_endpoint_matches_go_scope_without_fastpath_lookup(http_fsb, monkeypatch, mode):
    client, _, service = http_fsb
    service._app_config.ingress = _gateway_config(mode)
    fastpath = Mock(spec=FastPathClient)
    monkeypatch.setattr(service, "_fastpath", fastpath)
    previous = get_current_tenant()
    set_current_tenant(TenantEntry(name="tenant-a", namespace="tenant-a"))
    try:
        response = client.get(
            "/v1/sandboxes/sandbox-123/endpoints/44772",
            headers={"X-Namespace": "attacker"},
        )
    finally:
        set_current_tenant(previous)
    assert response.status_code == 200
    scope = "f1.dGVuYW50LWE.c2FuZGJveC0xMjM.44772.k.gtJzW337dCO-kStxh2GPfA"
    body = response.json()
    if mode == "header":
        assert body == {
            "endpoint": "ingress.example.com",
            "headers": {"OpenSandbox-Ingress-To": scope},
        }
    else:
        assert body["endpoint"] == f"ingress.example.com/{scope}"
        assert not body.get("headers")
    assert fastpath.mock_calls == []


@pytest.mark.parametrize("port", [8080, 18080])
def test_endpoint_binds_port_and_default_namespace(http_fsb, port):
    client, _, service = http_fsb
    service._app_config.ingress = _gateway_config()
    response = client.get(f"/v1/sandboxes/fsb-123/endpoints/{port}")
    assert response.status_code == 200
    scope = response.json()["headers"]["OpenSandbox-Ingress-To"]
    assert scope.startswith(f"f1.bnMtMQ.ZnNiLTEyMw.{port}.k.")


@pytest.mark.parametrize(
    "invalid", ["no_gateway", "no_keys", "wildcard", "expires", "port", "identity"]
)
def test_endpoint_rejects_unsupported_or_invalid_routes(http_fsb, invalid):
    _, _, service = http_fsb
    config = service._app_config
    config.ingress = _gateway_config("wildcard" if invalid == "wildcard" else "header")
    if invalid == "no_gateway":
        config.ingress = None
    elif invalid == "no_keys":
        config.ingress.secure_access = None
    with pytest.raises(HTTPException) as exc_info:
        service.get_endpoint(
            "bad\nidentity" if invalid == "identity" else "fsb-123",
            0 if invalid == "port" else 44772,
            expires=2_000_000_000 if invalid == "expires" else None,
        )
    assert exc_info.value.status_code == 400


def test_fsb_snapshot_operations_rejected(http_fsb):
    """Snapshot operations on fsb (fsb-) sandboxes are explicitly rejected
    at the service layer even though the kubernetes snapshot runtime is
    available for container-sandbox workloads."""
    client, fake, service = http_fsb
    sandbox_id = client.post(
        "/v1/sandboxes",
        json={
            "image": {"uri": "python:3.11"},
            "entrypoint": ["python"],
            "timeout": 3600,
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
        },
    ).json()["id"]

    response = client.post(f"/v1/sandboxes/{sandbox_id}/snapshots")
    assert response.status_code in (400, 404, 501)


def test_background_renew_resolves_tenant_namespace(http_fsb):
    client, fake, service = http_fsb
    sandbox_id = client.post(
        "/v1/sandboxes",
        json={
            "image": {"uri": "python:3.11"},
            "entrypoint": ["python"],
            "timeout": 3600,
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
        },
    ).json()["id"]
    sandbox = fake.sandboxes.pop(("ns-1", sandbox_id))
    fake.sandboxes[("tenant-a", sandbox_id)] = sandbox
    cr = fake.crs.pop(("ns-1", sandbox_id))
    cr["metadata"]["namespace"] = "tenant-a"
    fake.crs[("tenant-a", sandbox_id)] = cr
    service.set_tenant_provider(
        SimpleNamespace(list_tenants=lambda: [SimpleNamespace(namespace="tenant-a")])
    )

    service.renew_expiration(
        sandbox_id,
        request=RenewSandboxExpirationRequest(
            expiresAt=datetime.now(timezone.utc) + timedelta(hours=2)
        ),
    )

    assert fake.last_get.sandbox.namespaced_name.namespace == "tenant-a"
    assert fake.last_update.sandbox.namespaced_name.namespace == "tenant-a"


def test_background_lookup_does_not_treat_fastpath_failure_as_namespace_miss(http_fsb):
    _, fake, service = http_fsb
    fake.get_error_by_namespace["ns-1"] = grpc.StatusCode.UNAVAILABLE
    service.set_tenant_provider(
        SimpleNamespace(list_tenants=lambda: [SimpleNamespace(namespace="tenant-a")])
    )

    with pytest.raises(HTTPException) as exc_info:
        service.renew_expiration(
            "fsb-1",
            request=RenewSandboxExpirationRequest(
                expiresAt=datetime.now(timezone.utc) + timedelta(hours=2)
            ),
        )

    assert exc_info.value.status_code == 503


def test_http_create_returns_503_when_fastpath_is_unavailable(monkeypatch):
    config = AppConfig(
        server=ServerConfig(host="0.0.0.0", port=8080, api_key="x"),
        runtime=RuntimeConfig(type="kubernetes", execd_image="ghcr.io/opensandbox/execd:latest"),
        kubernetes=KubernetesRuntimeConfig(
            namespace="ns-1",
            fastpath_endpoint="127.0.0.1:1",
            fastpath_timeout_seconds=1,
        ),
    )
    fastpath = FastPathClient(endpoint="127.0.0.1:1", timeout_seconds=1)
    service = FastSandboxService(config, fastpath_client=fastpath)
    monkeypatch.setattr(lifecycle, "sandbox_service", service)
    app = FastAPI()
    app.include_router(lifecycle.router, prefix="/v1")

    try:
        with TestClient(app) as client:
            response = client.post(
                "/v1/sandboxes",
                json={
                    "image": {"uri": "python:3.11"},
                    "entrypoint": ["python"],
                    "timeout": 3600,
                    "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
                },
            )
            assert response.status_code == 503
    finally:
        fastpath.close()


@pytest.mark.parametrize("sandbox_id,is_fsb", [("fsb-123", True), ("flt-123", False)])
def test_composite_routes_only_fsb_prefix(sandbox_id, is_fsb):
    kubernetes = Mock(spec=KubernetesSandboxService)
    fsb = Mock(spec=FastSandboxService)
    service = CompositeSandboxService(kubernetes, fsb)
    expected, other = (fsb, kubernetes) if is_fsb else (kubernetes, fsb)

    assert service.get_sandbox(sandbox_id) is expected.get_sandbox.return_value
    expected.get_sandbox.assert_called_once_with(sandbox_id)
    other.get_sandbox.assert_not_called()


def test_fsb_list_excludes_old_prefix(persisted_fsb):
    client, fake, _, sandbox_id = persisted_fsb
    old = deepcopy(fake.crs[("ns-1", sandbox_id)])
    old["metadata"]["name"] = "flt-123"
    fake.crs[("ns-1", "flt-123")] = old

    response = client.get("/v1/sandboxes")
    assert response.status_code == 200
    assert [item["id"] for item in response.json()["items"]] == [sandbox_id]
