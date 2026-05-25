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
from __future__ import annotations

import json
from datetime import timedelta

import httpx
import pytest

from opensandbox.adapters.command_adapter import CommandsAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import InvalidArgumentException, SandboxApiException
from opensandbox.models.sandboxes import SandboxEndpoint


class _SseTransport(httpx.AsyncBaseTransport):
    def __init__(self) -> None:
        self.last_request: httpx.Request | None = None

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        self.last_request = request
        body = request.content.decode("utf-8") if isinstance(request.content, (bytes, bytearray)) else ""
        payload = json.loads(body) if body else {}

        if request.url.path == "/command" and payload.get("command") == "echo hi":
            sse = (
                b'data: {"type":"init","text":"exec-1","timestamp":1}\n\n'
                b'\n'
                b'data: {"type":"stdout","text":"hi","timestamp":2}\n\n'
                b"not-json\n\n"
                b'data: {"type":"result","results":{"text":"ok"},"timestamp":3}\n\n'
                b'data: {"type":"execution_complete","timestamp":4,"execution_time":5}\n\n'
            )
            return httpx.Response(
                200,
                headers={"Content-Type": "text/event-stream"},
                content=sse,
                request=request,
            )

        if request.url.path == "/session/sess-1/run" and payload.get("command") == "pwd":
            sse = (
                b'event: stdout\n'
                b'data: {"type":"stdout","text":"/var","timestamp":1}\n\n'
                b'event: execution_complete\n'
                b'data: {"type":"execution_complete","timestamp":2,"execution_time":3}\n\n'
            )
            return httpx.Response(
                200,
                headers={"Content-Type": "text/event-stream"},
                content=sse,
                request=request,
            )

        if request.url.path == "/session/sess-2/run" and payload.get("command") == "exit 7":
            sse = (
                b'data: {"type":"init","text":"sess-exec-2","timestamp":1}\n\n'
                b'data: {"type":"error","error":{"ename":"CommandExecError","evalue":"7","traceback":["exit status 7"]},"timestamp":2}\n\n'
            )
            return httpx.Response(
                200,
                headers={"Content-Type": "text/event-stream"},
                content=sse,
                request=request,
            )

        if request.url.path == "/command" and payload.get("command") == "exit null":
            sse = (
                b'data: {"type":"init","text":"exec-null","timestamp":1}\n\n'
                b'data: {"type":"error","error":{"ename":"CommandExecError","evalue":"fork/exec /usr/bin/bash: resource temporarily unavailable","traceback":null},"timestamp":2}\n\n'
            )
            return httpx.Response(
                200,
                headers={"Content-Type": "text/event-stream"},
                content=sse,
                request=request,
            )

        return httpx.Response(500, content=b"boom", request=request)


@pytest.mark.asyncio
async def test_run_command_streaming_happy_path_updates_execution() -> None:
    transport = _SseTransport()
    cfg = ConnectionConfig(protocol="http", transport=transport)
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapter(cfg, endpoint)

    execution = await adapter.run("echo hi")
    assert execution.id == "exec-1"
    assert execution.logs.stdout[0].text == "hi"
    assert execution.result[0].text == "ok"
    assert execution.complete is not None
    assert execution.complete.execution_time_in_millis == 5
    assert execution.exit_code == 0

    assert transport.last_request is not None
    assert transport.last_request.headers.get("accept") == "text/event-stream"


@pytest.mark.asyncio
async def test_run_command_streaming_non_zero_exit_updates_exit_code() -> None:
    class _ErrorTransport(httpx.AsyncBaseTransport):
        async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
            sse = (
                b'data: {"type":"init","text":"exec-2","timestamp":1}\n\n'
                b'data: {"type":"error","error":{"ename":"CommandExecError","evalue":"7","traceback":["exit status 7"]},"timestamp":2}\n\n'
            )
            return httpx.Response(
                200,
                headers={"Content-Type": "text/event-stream"},
                content=sse,
                request=request,
            )

    cfg = ConnectionConfig(protocol="http", transport=_ErrorTransport())
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapter(cfg, endpoint)

    execution = await adapter.run("exit 7")
    assert execution.id == "exec-2"
    assert execution.error is not None
    assert execution.error.value == "7"
    assert execution.complete is None
    assert execution.exit_code == 7


@pytest.mark.asyncio
async def test_run_command_streaming_tolerates_null_traceback() -> None:
    cfg = ConnectionConfig(protocol="http", transport=_SseTransport())
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapter(cfg, endpoint)

    execution = await adapter.run("exit null")

    assert execution.id == "exec-null"
    assert execution.error is not None
    assert execution.error.value == "fork/exec /usr/bin/bash: resource temporarily unavailable"
    assert execution.error.traceback == []
    assert execution.complete is None


@pytest.mark.asyncio
async def test_run_command_rejects_blank_command() -> None:
    cfg = ConnectionConfig(protocol="http")
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapter(cfg, endpoint)

    with pytest.raises(InvalidArgumentException):
        await adapter.run("   ")


@pytest.mark.asyncio
async def test_run_command_non_200_raises_api_exception() -> None:
    transport = _SseTransport()
    cfg = ConnectionConfig(protocol="http", transport=transport)
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapter(cfg, endpoint)

    with pytest.raises(SandboxApiException):
        await adapter.run("other")


@pytest.mark.asyncio
async def test_run_in_session_streaming_uses_generated_fields_and_exit_code() -> None:
    transport = _SseTransport()
    cfg = ConnectionConfig(protocol="http", transport=transport)
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapter(cfg, endpoint)

    execution = await adapter.run_in_session(
        "sess-1",
        "pwd",
        working_directory="/var",
        timeout=timedelta(seconds=5),
    )

    assert execution.logs.stdout[0].text == "/var"
    assert execution.complete is not None
    assert execution.complete.execution_time_in_millis == 3
    assert execution.exit_code == 0

    assert transport.last_request is not None
    assert transport.last_request.url.path == "/session/sess-1/run"
    request_body = json.loads(transport.last_request.content.decode("utf-8"))
    assert request_body == {
        "command": "pwd",
        "cwd": "/var",
        "timeout": 5000,
    }


@pytest.mark.asyncio
async def test_run_in_session_non_zero_exit_updates_exit_code() -> None:
    cfg = ConnectionConfig(protocol="http", transport=_SseTransport())
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapter(cfg, endpoint)

    execution = await adapter.run_in_session("sess-2", "exit 7")

    assert execution.id == "sess-exec-2"
    assert execution.error is not None
    assert execution.error.value == "7"
    assert execution.complete is None
    assert execution.exit_code == 7
