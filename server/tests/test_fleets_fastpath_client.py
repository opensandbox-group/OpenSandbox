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

"""Focused tests for the synchronous FastPath v2 gRPC adapter."""

from concurrent import futures
from threading import Event, Thread

import grpc
import pytest

from opensandbox_server.services.fleets import fastpath_client
from opensandbox_server.services.fleets.fastpath_client import (
    FastPathClient,
    FastPathConflict,
    FastPathError,
    FastPathInvalidArgument,
    FastPathNotFound,
    FastPathUnavailable,
    component_target,
    namespaced_reference,
    port_target,
)
from opensandbox_server.services.fleets.generated import fastpath_pb2 as pb2
from opensandbox_server.services.fleets.generated import fastpath_pb2_grpc as pb2_grpc


def _sandbox(name: str = "sbx-1") -> pb2.SandboxInfo:
    return pb2.SandboxInfo(
        identity=pb2.SandboxIdentity(uid=f"uid-{name}", name=name, namespace="ns-1"),
        applied_generation=3,
        runtime=pb2.RuntimeInfo(state=pb2.RUNTIME_STATE_READY),
        data_plane=pb2.DataPlaneInfo(state=pb2.DATA_PLANE_STATE_READY),
        ready=True,
    )


class _FakeFastPathService(pb2_grpc.FastPathServiceServicer):
    def __init__(self):
        self.created: list[pb2.CreateSandboxRequest] = []
        self.last_get: pb2.GetSandboxRequest | None = None
        self.last_delete: pb2.DeleteRequest | None = None
        self.updates: list[pb2.UpdateSandboxRequest] = []
        self.last_list: pb2.ListSandboxesRequest | None = None
        self.last_resolve: pb2.ResolveEndpointRequest | None = None
        self.get_error: grpc.StatusCode | None = None
        self.create_time_remaining: float | None = None

    def CreateSandbox(self, request, context):
        self.created.append(request)
        self.create_time_remaining = context.time_remaining()
        return pb2.CreateSandboxResponse(
            sandbox=_sandbox(request.request_id),
            generation=3,
            completion=request.completion,
        )

    def GetSandbox(self, request, context):
        self.last_get = request
        if self.get_error is not None:
            context.abort(self.get_error, "scripted failure")
        return pb2.GetSandboxResponse(
            sandbox=_sandbox(request.sandbox.namespaced_name.name), generation=3
        )

    def DeleteSandbox(self, request, context):
        self.last_delete = request
        return pb2.DeleteResponse()

    def ListSandboxes(self, request, context):
        self.last_list = request
        return pb2.ListSandboxesResponse(
            items=[pb2.SandboxSummary(identity=_sandbox().identity, generation=3)]
        )

    def UpdateSandbox(self, request, context):
        self.updates.append(request)
        return pb2.UpdateSandboxResponse(sandbox=_sandbox().identity, committed_generation=4)

    def GetSandboxDiagnostics(self, request, context):
        return pb2.SandboxDiagnosticsResponse(sandbox=_sandbox(), assignment_state="assigned")

    def ResolveEndpoint(self, request, context):
        self.last_resolve = request
        return pb2.ResolveEndpointResponse(
            sandbox_uid="uid-sbx-1",
            endpoint=pb2.ResolvedEndpoint(component_name="execd", protocol="HTTP", port=44772),
            proxy_endpoint="http://sandbox-proxy:8080",
            route_generation=1,
            required_headers={"x-fast-sandbox-route-credential": "token-1"},
        )

    def GetPool(self, request, context):
        return pb2.PoolInfo(namespace=request.namespace, name=request.pool_name)

    def ListPools(self, request, context):
        return pb2.ListPoolsResponse(
            items=[pb2.PoolInfo(namespace=request.namespace, name="default-pool")]
        )


@pytest.fixture
def client_and_server():
    service = _FakeFastPathService()
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=4))
    pb2_grpc.add_FastPathServiceServicer_to_server(service, server)
    port = server.add_insecure_port("127.0.0.1:0")
    server.start()
    client = FastPathClient(endpoint=f"127.0.0.1:{port}")
    try:
        yield client, service
    finally:
        client.close()
        server.stop(None)


def test_lifecycle_requests_use_new_contract_and_fences(client_and_server):
    client, service = client_and_server
    create = pb2.CreateSandboxRequest(
        request_id="sbx-1",
        namespace="ns-1",
        image="python:3.11",
        pool_ref="default-pool",
        completion=pb2.CREATE_COMPLETION_READY,
    )

    created = client.create_sandbox(create, wait_timeout_millis=45000)
    fetched = client.get_sandbox("ns-1", "sbx-1", expected_uid="uid-sbx-1", expected_generation=3)
    client.update_expiration(
        "ns-1",
        "sbx-1",
        1750000000,
        expected_uid="uid-sbx-1",
        expected_generation=3,
    )
    client.update_metadata(
        "ns-1",
        "sbx-1",
        upsert={"team": "agents"},
        delete_keys=["old"],
        expected_uid="uid-sbx-1",
        expected_generation=3,
    )
    client.delete_sandbox("ns-1", "sbx-1", expected_uid="uid-sbx-1")

    assert created.sandbox.identity.name == "sbx-1"
    assert created.completion == pb2.CREATE_COMPLETION_READY
    assert service.created[0].image == "python:3.11"
    assert service.create_time_remaining is not None
    assert 45 < service.create_time_remaining <= 50
    assert fetched.generation == 3
    assert service.last_get.sandbox.expected_uid == "uid-sbx-1"
    assert service.last_get.expected_generation == 3
    assert service.updates[0].sandbox.expected_uid == "uid-sbx-1"
    assert service.updates[0].expected_generation == 3
    assert service.updates[0].expires_at_unix_seconds == 1750000000
    assert service.updates[1].sandbox.expected_uid == "uid-sbx-1"
    assert service.updates[1].expected_generation == 3
    assert service.updates[1].metadata_upsert == {"team": "agents"}
    assert list(service.updates[1].metadata_delete_keys) == ["old"]
    assert service.last_delete.sandbox.expected_uid == "uid-sbx-1"


def test_list_diagnostics_endpoint_and_pool_calls(client_and_server):
    client, service = client_and_server

    listed = client.list_sandboxes(
        "ns-1", metadata={"team": "agents"}, page_size=10, page_token="tok"
    )
    diagnostics = client.get_sandbox_diagnostics("ns-1", "sbx-1")
    resolved = client.resolve_endpoint(
        namespaced_reference("ns-1", "sbx-1", expected_uid="uid-sbx-1"),
        component_target("execd"),
        expected_generation=3,
    )
    raw_target = port_target(8080)

    assert listed.items[0].identity.name == "sbx-1"
    assert service.last_list.namespace == "ns-1"
    assert service.last_list.metadata == {"team": "agents"}
    assert service.last_list.page_size == 10
    assert service.last_list.page_token == "tok"
    assert diagnostics.assignment_state == "assigned"
    assert resolved.endpoint.port == 44772
    assert service.last_resolve.sandbox.expected_uid == "uid-sbx-1"
    assert service.last_resolve.expected_generation == 3
    assert raw_target.port == 8080
    assert client.get_pool("ns-1", "default-pool").name == "default-pool"
    assert client.list_pools("ns-1").items[0].name == "default-pool"


def test_not_found_is_typed(client_and_server):
    client, service = client_and_server
    service.get_error = grpc.StatusCode.NOT_FOUND

    with pytest.raises(FastPathNotFound):
        client.get_sandbox("ns-1", "missing")


def test_error_mapping_covers_common_codes():
    cases = [
        (grpc.StatusCode.NOT_FOUND, FastPathNotFound),
        (grpc.StatusCode.INVALID_ARGUMENT, FastPathInvalidArgument),
        (grpc.StatusCode.ALREADY_EXISTS, FastPathConflict),
        (grpc.StatusCode.ABORTED, FastPathConflict),
        (grpc.StatusCode.UNAVAILABLE, FastPathUnavailable),
        (grpc.StatusCode.DEADLINE_EXCEEDED, FastPathUnavailable),
        (grpc.StatusCode.CANCELLED, FastPathUnavailable),
        (grpc.StatusCode.PERMISSION_DENIED, FastPathError),
    ]
    for code, expected in cases:
        error = fastpath_client._to_fastpath_error(_ScriptedRpcError(code))
        assert isinstance(error, expected)
        assert error.code == code.name


def test_concurrent_first_use_waits_for_stub_publication(monkeypatch):
    channel = object()
    stub_started = Event()
    release_stub = Event()
    errors: list[Exception] = []

    monkeypatch.setattr(grpc, "insecure_channel", lambda _endpoint: channel)

    def build_stub(_channel):
        stub_started.set()
        assert release_stub.wait(timeout=5)
        return object()

    monkeypatch.setattr(fastpath_client.fastpath_pb2_grpc, "FastPathServiceStub", build_stub)
    client = FastPathClient()

    first = Thread(target=client.connect)
    second = Thread(target=lambda: _capture_stub_error(client, errors))
    first.start()
    assert stub_started.wait(timeout=5)
    second.start()
    release_stub.set()
    first.join(timeout=5)
    second.join(timeout=5)

    assert not first.is_alive()
    assert not second.is_alive()
    assert errors == []


def _capture_stub_error(client: FastPathClient, errors: list[Exception]) -> None:
    try:
        client._require_stub()
    except Exception as exc:  # noqa: BLE001 - the assertion needs the exact failure
        errors.append(exc)


class _ScriptedRpcError(grpc.RpcError):
    def __init__(self, code: grpc.StatusCode):
        self._code = code

    def code(self) -> grpc.StatusCode:
        return self._code

    def details(self) -> str:
        return f"scripted {self._code.name}"
