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

"""Unit tests for the FastPath v2 gRPC client."""

import grpc
import pytest
import pytest_asyncio
from grpc import aio

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
from opensandbox_server.services.fleets.generated import (
    fastpath_pb2 as pb2,
)
from opensandbox_server.services.fleets.generated import (
    fastpath_pb2_grpc as pb2_grpc,
)


class _FakeFastPathService(pb2_grpc.FastPathServiceServicer):
    """In-process FastPath server with scripted responses."""

    def __init__(self):
        self.created: list[pb2.CreateRequest] = []
        self.last_delete: tuple[str, str] | None = None
        self.sandbox = pb2.SandboxInfo(
            sandbox_uid="uid-1",
            sandbox_name="sbx-1",
            namespace="ns-1",
            runtime_state="Ready",
            data_plane_state="Ready",
            image="python:3.11",
            pool_ref="default-pool",
        )
        self.get_error: grpc.StatusCode | None = None

    async def CreateSandbox(self, request, context):
        self.created.append(request)
        info = pb2.SandboxInfo()
        info.CopyFrom(self.sandbox)
        info.sandbox_name = request.request_id
        info.namespace = request.namespace
        return info

    async def GetSandbox(self, request, context):
        if self.get_error is not None:
            await context.abort(self.get_error, "scripted failure")
        info = pb2.SandboxInfo()
        info.CopyFrom(self.sandbox)
        info.sandbox_name = request.sandbox_name
        info.namespace = request.namespace
        return info

    async def DeleteSandbox(self, request, context):
        self.last_delete = (request.namespace, request.sandbox_name)
        return pb2.DeleteResponse(success=True)

    async def ListSandboxes(self, request, context):
        response = pb2.ListResponse()
        info = pb2.SandboxInfo()
        info.CopyFrom(self.sandbox)
        info.namespace = request.namespace
        response.items.append(info)
        return response

    async def UpdateSandbox(self, request, context):
        return pb2.UpdateResponse(
            success=True,
            sandbox=pb2.SandboxInfo(
                sandbox_uid="uid-1",
                sandbox_name=request.sandbox_name,
                namespace=request.namespace,
                runtime_state="Ready",
                data_plane_state="Ready",
                expires_at_unix_seconds=request.expires_at_unix_seconds,
            ),
        )

    async def GetSandboxDiagnostics(self, request, context):
        return pb2.SandboxDiagnosticsResponse(
            sandbox=self.sandbox, assignment_state="assigned"
        )

    async def WaitSandboxReady(self, request, context):
        info = pb2.SandboxInfo()
        info.CopyFrom(self.sandbox)
        return info

    async def ResolveEndpoint(self, request, context):
        return pb2.ResolveEndpointResponse(
            sandbox_uid="uid-1",
            protocol="HTTP",
            resolved_port=44772,
            proxy_endpoint="http://sandbox-proxy:8080",
            route_generation=1,
            expires_at_unix_seconds=1750000000,
            required_headers={"x-fast-sandbox-route-credential": "token-1"},
        )

    async def GetPool(self, request, context):
        return pb2.PoolInfo(namespace=request.namespace, name=request.pool_name)

    async def ListPools(self, request, context):
        return pb2.ListPoolsResponse(
            items=[pb2.PoolInfo(namespace=request.namespace, name="default-pool")]
        )


@pytest_asyncio.fixture
async def client_and_server():
    service = _FakeFastPathService()
    server = aio.server()
    pb2_grpc.add_FastPathServiceServicer_to_server(service, server)
    port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    channel = aio.insecure_channel(f"127.0.0.1:{port}")
    stub = pb2_grpc.FastPathServiceStub(channel)
    client = FastPathClient(endpoint=f"127.0.0.1:{port}")
    client._channel = channel  # noqa: SLF001 - share the fixture channel
    client._stub = stub
    try:
        yield client, service
    finally:
        await channel.close()
        await server.stop(None)


@pytest.mark.asyncio
async def test_create_sandbox_passes_through_fields(client_and_server):
    client, service = client_and_server
    request = pb2.CreateRequest(
        request_id="sbx-1",
        namespace="ns-1",
        image="python:3.11",
        pool_ref="default-pool",
        command=["python", "-m", "http.server"],
        args=["8000"],
        envs={"PYTHONUNBUFFERED": "1"},
        expires_at_unix_seconds=1750000000,
    )
    request.metadata.update({"team": "agents"})

    info = await client.create_sandbox(request)

    assert info.sandbox_name == "sbx-1"
    assert info.namespace == "ns-1"
    assert service.created[-1].image == "python:3.11"
    assert service.created[-1].expires_at_unix_seconds == 1750000000
    assert service.created[-1].envs["PYTHONUNBUFFERED"] == "1"


@pytest.mark.asyncio
async def test_get_sandbox_round_trips_identity(client_and_server):
    client, _ = client_and_server
    info = await client.get_sandbox("ns-1", "sbx-1")
    assert info.sandbox_name == "sbx-1"
    assert info.namespace == "ns-1"


@pytest.mark.asyncio
async def test_get_sandbox_not_found_maps_to_fastpath_not_found(client_and_server):
    client, service = client_and_server
    service.get_error = grpc.StatusCode.NOT_FOUND
    with pytest.raises(FastPathNotFound) as exc_info:
        await client.get_sandbox("ns-1", "missing")
    assert exc_info.value.code == "NOT_FOUND"


@pytest.mark.asyncio
async def test_delete_sandbox_passes_namespace_and_name(client_and_server):
    client, service = client_and_server
    await client.delete_sandbox("ns-1", "sbx-1")
    assert service.last_delete == ("ns-1", "sbx-1")


@pytest.mark.asyncio
async def test_list_sandboxes_applies_filter_and_paging(client_and_server):
    client, _ = client_and_server
    response = await client.list_sandboxes(
        "ns-1", metadata={"team": "agents"}, page_size=10, page_token="tok"
    )
    assert response.items[0].namespace == "ns-1"


@pytest.mark.asyncio
async def test_update_expiration_returns_sandbox(client_and_server):
    client, _ = client_and_server
    info = await client.update_expiration("ns-1", "sbx-1", 1750000000)
    assert info.expires_at_unix_seconds == 1750000000


@pytest.mark.asyncio
async def test_update_metadata_upsert_and_delete_keys(client_and_server):
    client, _ = client_and_server
    info = await client.update_metadata(
        "ns-1", "sbx-1", upsert={"a": "1"}, delete_keys=["b"]
    )
    assert info.sandbox_name == "sbx-1"


@pytest.mark.asyncio
async def test_diagnostics_and_ready_and_endpoint_calls(client_and_server):
    client, _ = client_and_server

    diag = await client.get_sandbox_diagnostics("ns-1", "sbx-1", limit=10)
    assert diag.assignment_state == "assigned"

    ready = await client.wait_sandbox_ready(
        namespaced_reference("ns-1", "sbx-1"), data_plane=True
    )
    assert ready.runtime_state == "Ready"

    resolved = await client.resolve_endpoint(
        namespaced_reference("ns-1", "sbx-1"), component_target("execd")
    )
    assert resolved.resolved_port == 44772
    assert resolved.required_headers["x-fast-sandbox-route-credential"] == "token-1"


@pytest.mark.asyncio
async def test_pool_calls(client_and_server):
    client, _ = client_and_server
    pool = await client.get_pool("ns-1", "default-pool")
    assert pool.name == "default-pool"
    pools = await client.list_pools("ns-1")
    assert pools.items[0].name == "default-pool"


class _TimeoutRecordingStub:
    """Wrapper stub recording the transport deadline of each call."""

    def __init__(self, inner):
        self._inner = inner
        self.deadlines: list[float | None] = []

    def _record(self, method_name):
        async def call(request, timeout=None):
            self.deadlines.append(timeout)
            return await getattr(self._inner, method_name)(request, None)

        return call

    def __getattr__(self, name):
        return self._record(name)


@pytest.mark.asyncio
async def test_deadline_accounts_for_readiness_wait(client_and_server):
    client, service = client_and_server
    recording = _TimeoutRecordingStub(service)
    client._stub = recording  # noqa: SLF001
    ref = namespaced_reference("ns-1", "sbx-1")

    # Non-waiting endpoint lookups stay on the configured deadline even when a
    # large server-side wait window is supplied.
    await client.resolve_endpoint(
        ref, component_target("execd"), wait_until_ready=False, wait_timeout_millis=60000
    )
    assert recording.deadlines[-1] == 30.0

    # Wait-enabled calls extend the deadline beyond the server-side wait.
    await client.resolve_endpoint(
        ref, component_target("execd"), wait_until_ready=True, wait_timeout_millis=60000
    )
    assert recording.deadlines[-1] == 65.0

    await client.wait_sandbox_ready(ref, data_plane=True, wait_timeout_millis=60000)
    assert recording.deadlines[-1] == 65.0

    # Plain lifecycle calls always use the configured deadline.
    await client.get_sandbox("ns-1", "sbx-1")
    assert recording.deadlines[-1] == 30.0


@pytest.mark.asyncio
async def test_error_mapping_covers_common_codes():
    cases = [
        (grpc.StatusCode.NOT_FOUND, FastPathNotFound),
        (grpc.StatusCode.INVALID_ARGUMENT, FastPathInvalidArgument),
        (grpc.StatusCode.ALREADY_EXISTS, FastPathConflict),
        (grpc.StatusCode.UNAVAILABLE, FastPathUnavailable),
        (grpc.StatusCode.DEADLINE_EXCEEDED, FastPathUnavailable),
        (grpc.StatusCode.PERMISSION_DENIED, FastPathError),
    ]
    for code, expected in cases:
        error = fastpath_client._to_fastpath_error(  # noqa: SLF001
            _abort_error(code)
        )
        assert isinstance(error, expected)
        assert error.code == code.name


@pytest.mark.asyncio
async def test_client_requires_connect_before_calls():
    client = FastPathClient(endpoint="127.0.0.1:1")
    with pytest.raises(FastPathUnavailable):
        await client.create_sandbox(pb2.CreateRequest(request_id="x"))


def test_reference_and_target_helpers():
    ref = namespaced_reference("ns-1", "sbx-1")
    assert ref.namespaced_name.namespace == "ns-1"
    assert ref.namespaced_name.name == "sbx-1"

    assert component_target("execd").component_name == "execd"
    assert port_target(8000).port == 8000


def _abort_error(code: grpc.StatusCode) -> aio.AioRpcError:
    """Build a minimal AioRpcError carrying only the status code."""
    return aio.AioRpcError(
        code,
        aio.Metadata(),  # initial_metadata
        aio.Metadata(),  # trailing_metadata
        f"scripted {code.name}",
    )
