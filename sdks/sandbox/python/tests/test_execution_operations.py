#
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
#
import asyncio
import json
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from datetime import timedelta

import httpx
import pytest

from opensandbox.adapters.command_adapter import CommandsAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.execd import RunCommandOpts
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.command_adapter import CommandsAdapterSync


def transport_handler(request):
    if request.url.path == "/execution/instance":
        return httpx.Response(
            200,
            json={
                "instance_id": "scope",
                "issued_at": 123,
                "retention_seconds": 86400,
                "capacity": 4096,
            },
        )
    if request.method == "POST":
        body = json.loads(request.content)
        assert body["operation_id"] == "scope.123.persisted"
        assert body["command"] == "echo hello"
        assert body["cwd"] == "/tmp"
        if request.url.path == "/command/operations":
            assert body["timeout"] == 2000
            assert body["envs"] == {"A": "value"}
    else:
        assert request.headers["X-EXECD-OPERATION-ID"] == "scope.123.persisted"
        if request.url.params["kind"] == "pty":
            return httpx.Response(
                409,
                json={
                    "code": "operation_instance_mismatch",
                    "message": "unknown outcome",
                },
            )
    return httpx.Response(
        202,
        json={
            "id": "original",
            "kind": "command",
            "state": "creating",
            "expires_at": "2026-09-09T00:00:00Z",
        },
    )


@pytest.mark.parametrize("launch", [None, (False, False), (True, True), (True, False)])
def test_pty_status_client_distinguishes_missing_launch_outcome(launch):
    from opensandbox.api.execd.api.command import get_pty_session_status
    from opensandbox.api.execd.client import Client
    from opensandbox.api.execd.types import UNSET

    payload = {"session_id": "recovered", "running": False, "output_offset": 0}
    if launch is not None:
        payload.update(launch_attempted=launch[0], launch_failed=launch[1])
    with httpx.Client(
        base_url="http://execd",
        transport=httpx.MockTransport(lambda _: httpx.Response(200, json=payload)),
    ) as http_client:
        client = Client(base_url="http://execd").set_httpx_client(http_client)
        result = get_pty_session_status.sync("recovered", client=client)
    assert result is not None
    if launch is None:
        assert result.launch_attempted is UNSET
        assert result.launch_failed is UNSET
    else:
        assert result.launch_attempted is launch[0]
        assert result.launch_failed is launch[1]


@pytest.mark.asyncio
async def test_async_execution_operations():
    adapter = CommandsAdapter(
        ConnectionConfig(), SandboxEndpoint(endpoint="localhost:44772")
    )
    client = httpx.AsyncClient(
        transport=httpx.MockTransport(transport_handler),
        base_url="http://localhost:44772",
    )
    adapter._client.set_async_httpx_client(client)
    try:
        from opensandbox.services.command import get_execution_operations

        assert get_execution_operations(adapter) is adapter
        instance = await adapter.get_execution_instance()
        assert instance.new_operation_id().startswith("scope.123.")
        opts = RunCommandOpts(
            working_directory="/tmp", timeout=timedelta(seconds=2), envs={"A": "value"}
        )
        op = await adapter.create_command_operation(
            "scope.123.persisted", "echo hello", opts=opts
        )
        assert op.id == "original" and op.state == "creating"
        assert (
            await adapter.get_execution_operation("command", "scope.123.persisted")
        ).id == op.id
        assert (
            await adapter.create_pty_operation(
                "scope.123.persisted", cwd="/tmp", command="echo hello"
            )
        ).id == op.id
        with pytest.raises(SandboxApiException):
            await adapter.get_execution_operation("pty", "scope.123.persisted")
    finally:
        await client.aclose()
        await adapter._httpx_client.aclose()
        await adapter._sse_client.aclose()


def test_sync_execution_operations():
    adapter = CommandsAdapterSync(
        ConnectionConfig(), SandboxEndpoint(endpoint="localhost:44772")
    )
    with httpx.Client(
        transport=httpx.MockTransport(transport_handler),
        base_url="http://localhost:44772",
    ) as client:
        adapter._client.set_httpx_client(client)
        from opensandbox.sync.services.command import get_execution_operations

        assert get_execution_operations(adapter) is adapter
        instance = adapter.get_execution_instance()
        assert instance.new_operation_id().startswith("scope.123.")
        opts = RunCommandOpts(
            working_directory="/tmp", timeout=timedelta(seconds=2), envs={"A": "value"}
        )
        op = adapter.create_command_operation(
            "scope.123.persisted", "echo hello", opts=opts
        )
        assert op.id == "original" and op.state == "creating"
        assert (
            adapter.get_execution_operation("command", "scope.123.persisted").id
            == op.id
        )
        assert (
            adapter.create_pty_operation(
                "scope.123.persisted", cwd="/tmp", command="echo hello"
            ).id
            == op.id
        )
        with pytest.raises(SandboxApiException):
            adapter.get_execution_operation("pty", "scope.123.persisted")
    adapter._httpx_client.close()
    adapter._sse_client.close()


@pytest.mark.parametrize(
    "method,args",
    [
        ("get_execution_instance", ()),
        ("get_execution_operation", ("command", "scope.123.persisted")),
        ("create_command_operation", ("scope.123.persisted", "true")),
        ("create_pty_operation", ("scope.123.persisted",)),
    ],
)
@pytest.mark.asyncio
async def test_authentication_error_mapping(method, args):
    # Matches the real router's operation-endpoint 401 contract.
    def unauthorized(request):
        return httpx.Response(
            401,
            json={
                "code": "UNAUTHORIZED",
                "message": "Invalid or missing execd access token",
            },
        )

    adapter = CommandsAdapter(
        ConnectionConfig(), SandboxEndpoint(endpoint="localhost:44772")
    )
    async with httpx.AsyncClient(
        transport=httpx.MockTransport(unauthorized), base_url="http://localhost:44772"
    ) as client:
        adapter._client.set_async_httpx_client(client)
        try:
            with pytest.raises(SandboxApiException):
                await getattr(adapter, method)(*args)
        finally:
            await adapter._httpx_client.aclose()
            await adapter._sse_client.aclose()
    sync = CommandsAdapterSync(
        ConnectionConfig(), SandboxEndpoint(endpoint="localhost:44772")
    )
    with httpx.Client(
        transport=httpx.MockTransport(unauthorized), base_url="http://localhost:44772"
    ) as client:
        sync._client.set_httpx_client(client)
        try:
            with pytest.raises(SandboxApiException):
                getattr(sync, method)(*args)
        finally:
            sync._httpx_client.close()
            sync._sse_client.close()


def test_recovery_capability_rejects_legacy_adapters():
    from typing import cast

    from opensandbox.services.command import Commands, get_execution_operations
    from opensandbox.sync.services.command import CommandsSync
    from opensandbox.sync.services.command import (
        get_execution_operations as sync_operations,
    )

    with pytest.raises(TypeError, match="does not support"):
        get_execution_operations(cast(Commands, object()))
    with pytest.raises(TypeError, match="does not support"):
        sync_operations(cast(CommandsSync, object()))


def instance_response(issued_at=123):
    return httpx.Response(
        200,
        json={
            "instance_id": "scope",
            "issued_at": issued_at,
            "retention_seconds": 86400,
            "capacity": 4096,
        },
    )


@pytest.mark.asyncio
async def test_async_instance_cache():
    entered, release = asyncio.Event(), asyncio.Event()
    calls = 0

    async def handler(request):
        nonlocal calls
        calls += 1
        entered.set()
        await release.wait()
        return instance_response(calls)

    adapter = CommandsAdapter(
        ConnectionConfig(), SandboxEndpoint(endpoint="localhost:44772")
    )
    async with httpx.AsyncClient(
        transport=httpx.MockTransport(handler), base_url="http://localhost:44772"
    ) as client:
        adapter._client.set_async_httpx_client(client)
        try:
            tasks = [
                asyncio.create_task(adapter.get_execution_instance()) for _ in range(16)
            ]
            await entered.wait()
            tasks[0].cancel()
            with pytest.raises(asyncio.CancelledError):
                await tasks[0]
            release.set()
            values = await asyncio.gather(*tasks[1:])
            assert calls == 1 and len({id(value) for value in values}) == 15
            values[0].instance_id = "caller-mutation"
            assert (await adapter.get_execution_instance()).instance_id == "scope"
            adapter._instance_cache = (time.monotonic() - 60, values[1])
            assert (await adapter.get_execution_instance()).issued_at == 2
        finally:
            release.set()
            await adapter._httpx_client.aclose()
            await adapter._sse_client.aclose()


def test_sync_instance_cache():
    entered, release = threading.Event(), threading.Event()
    calls = 0

    def handler(request):
        nonlocal calls
        calls += 1
        entered.set()
        assert release.wait(5)
        return instance_response(calls)

    adapter = CommandsAdapterSync(
        ConnectionConfig(), SandboxEndpoint(endpoint="localhost:44772")
    )
    with httpx.Client(
        transport=httpx.MockTransport(handler), base_url="http://localhost:44772"
    ) as client:
        adapter._client.set_httpx_client(client)
        try:
            with ThreadPoolExecutor(max_workers=16) as pool:
                tasks = [pool.submit(adapter.get_execution_instance) for _ in range(16)]
                assert entered.wait(5)
                release.set()
                values = [task.result(timeout=5) for task in tasks]
            assert calls == 1 and len({id(value) for value in values}) == 16
            values[0].instance_id = "caller-mutation"
            assert adapter.get_execution_instance().instance_id == "scope"
            adapter._instance_cache = (time.monotonic() - 60, values[1])
            assert adapter.get_execution_instance().issued_at == 2
        finally:
            release.set()
            adapter._httpx_client.close()
            adapter._sse_client.close()


@pytest.mark.parametrize("code", ["operation_instance_mismatch", "operation_expired"])
@pytest.mark.parametrize(
    "method,args",
    [
        ("create_command_operation", ("saved.identity", "true")),
        ("create_pty_operation", ("saved.identity",)),
        ("get_execution_operation", ("command", "saved.identity")),
    ],
)
@pytest.mark.asyncio
@pytest.mark.parametrize("is_async", [True, False])
async def test_instance_invalidation_without_replay(code, method, args, is_async):
    # Both adapters must discard an old in-flight result after invalidation.
    async_entered, async_release = asyncio.Event(), asyncio.Event()
    sync_entered, sync_release = threading.Event(), threading.Event()
    gets = 0
    operations = 0

    def response(request):
        nonlocal gets, operations
        if request.url.path == "/execution/instance":
            gets += 1
            return instance_response(gets)
        operations += 1
        if request.method == "POST":
            assert json.loads(request.content)["operation_id"] == "saved.identity"
        else:
            assert request.headers["X-EXECD-OPERATION-ID"] == "saved.identity"
        return httpx.Response(409, json={"code": code, "message": "unknown outcome"})

    async def async_handler(request):
        result = response(request)
        if request.url.path == "/execution/instance" and gets == 1:
            async_entered.set()
            await async_release.wait()
        return result

    def sync_handler(request):
        result = response(request)
        if request.url.path == "/execution/instance" and gets == 1:
            sync_entered.set()
            assert sync_release.wait(5)
        return result

    if is_async:
        adapter = CommandsAdapter(
            ConnectionConfig(), SandboxEndpoint(endpoint="localhost:44772")
        )
        async with httpx.AsyncClient(
            transport=httpx.MockTransport(async_handler),
            base_url="http://localhost:44772",
        ) as client:
            adapter._client.set_async_httpx_client(client)
            try:
                old = asyncio.create_task(adapter.get_execution_instance())
                await async_entered.wait()
                with pytest.raises(SandboxApiException):
                    await getattr(adapter, method)(*args)
                assert (await adapter.get_execution_instance()).issued_at == 2
                async_release.set()
                assert (await old).issued_at == 1
                assert (await adapter.get_execution_instance()).issued_at == 2
            finally:
                async_release.set()
                await adapter._httpx_client.aclose()
                await adapter._sse_client.aclose()
    else:
        adapter = CommandsAdapterSync(
            ConnectionConfig(), SandboxEndpoint(endpoint="localhost:44772")
        )
        with httpx.Client(
            transport=httpx.MockTransport(sync_handler),
            base_url="http://localhost:44772",
        ) as client:
            adapter._client.set_httpx_client(client)
            try:
                with ThreadPoolExecutor(max_workers=1) as pool:
                    old = pool.submit(adapter.get_execution_instance)
                    assert sync_entered.wait(5)
                    with pytest.raises(SandboxApiException):
                        getattr(adapter, method)(*args)
                    assert adapter.get_execution_instance().issued_at == 2
                    sync_release.set()
                    assert old.result(timeout=5).issued_at == 1
                    assert adapter.get_execution_instance().issued_at == 2
            finally:
                sync_release.set()
                adapter._httpx_client.close()
                adapter._sse_client.close()
    assert gets == 2 and operations == 1


@pytest.mark.asyncio
@pytest.mark.parametrize("is_async", [True, False])
async def test_instance_failure_is_not_cached(is_async):
    calls = 0

    def handler(request):
        nonlocal calls
        calls += 1
        if calls == 1:
            return httpx.Response(
                503, json={"code": "unavailable", "message": "retry later"}
            )
        return instance_response()

    if is_async:
        adapter = CommandsAdapter(
            ConnectionConfig(), SandboxEndpoint(endpoint="localhost:44772")
        )
        async with httpx.AsyncClient(
            transport=httpx.MockTransport(handler),
            base_url="http://localhost:44772",
        ) as client:
            adapter._client.set_async_httpx_client(client)
            try:
                with pytest.raises(SandboxApiException):
                    await adapter.get_execution_instance()
                assert (await adapter.get_execution_instance()).instance_id == "scope"
            finally:
                await adapter._httpx_client.aclose()
                await adapter._sse_client.aclose()
    else:
        adapter = CommandsAdapterSync(
            ConnectionConfig(), SandboxEndpoint(endpoint="localhost:44772")
        )
        with httpx.Client(
            transport=httpx.MockTransport(handler),
            base_url="http://localhost:44772",
        ) as client:
            adapter._client.set_httpx_client(client)
            try:
                with pytest.raises(SandboxApiException):
                    adapter.get_execution_instance()
                assert adapter.get_execution_instance().instance_id == "scope"
            finally:
                adapter._httpx_client.close()
                adapter._sse_client.close()
    assert calls == 2
