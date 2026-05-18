#
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
#
from __future__ import annotations

import json
from datetime import timedelta

import httpx

from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.command_adapter import CommandsAdapterSync


class _SyncErrorAfterStream(httpx.SyncByteStream):
    """Sync byte stream that yields data then raises ReadError."""

    def __init__(self, *chunks: bytes) -> None:
        self._chunks = iter(chunks)

    def __iter__(self):
        return self

    def __next__(self) -> bytes:
        try:
            chunk = next(self._chunks)
        except StopIteration:
            raise httpx.ReadError("simulated disconnect") from None
        if chunk is None:
            raise httpx.ReadError("simulated disconnect")
        return chunk


class _SyncResumeTransport(httpx.BaseTransport):
    """Simulates SSE disconnect then resume on GET /command/:id/resume (sync)."""

    def __init__(self) -> None:
        self.post_count = 0
        self.resume_count = 0
        self.last_resume_eid: int | None = None

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        if request.method == "POST" and request.url.path == "/command":
            self.post_count += 1
            partial = (
                b'data: {"type":"init","text":"cmd-resume-1","timestamp":100}\n\n',
                b'data: {"type":"stdout","eid":1,"text":"line1","timestamp":101}\n\n',
                b'data: {"type":"stdout","eid":2,"text":"line2","timestamp":102}\n\n',
                None,  # sentinel: disconnect after line2
            )
            return httpx.Response(
                200,
                headers={"Content-Type": "text/event-stream"},
                stream=_SyncErrorAfterStream(*partial),
                request=request,
            )

        if request.method == "GET" and "/resume" in request.url.path:
            self.resume_count += 1
            qp = request.url.params.get("after_eid")
            if qp:
                self.last_resume_eid = int(qp)
            remaining = (
                b'data: {"type":"stdout","eid":3,"text":"line3","timestamp":103}\n\n'
                b'data: {"type":"execution_complete","eid":4,"execution_time":42,"timestamp":104}\n\n'
            )
            return httpx.Response(
                200,
                headers={"Content-Type": "text/event-stream"},
                content=remaining,
                request=request,
            )

        return httpx.Response(500, content=b"unexpected", request=request)


class _SseTransport(httpx.BaseTransport):
    def handle_request(self, request: httpx.Request) -> httpx.Response:
        body = request.content.decode("utf-8") if isinstance(request.content, (bytes, bytearray)) else ""
        payload = json.loads(body) if body else {}

        if request.url.path == "/command" and payload.get("command") == "echo hi":
            sse = (
                b'data: {"type":"init","text":"exec-1","timestamp":1}\n\n'
                b'data: {"type":"stdout","text":"hi","timestamp":2}\n\n'
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


def test_sync_run_command_streaming_happy_path_updates_execution() -> None:
    cfg = ConnectionConfigSync(protocol="http", transport=_SseTransport())
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapterSync(cfg, endpoint)

    execution = adapter.run("echo hi")
    assert execution.id == "exec-1"
    assert execution.logs.stdout[0].text == "hi"
    assert execution.complete is not None
    assert execution.complete.execution_time_in_millis == 5
    assert execution.exit_code == 0


def test_sync_run_command_streaming_non_zero_exit_updates_exit_code() -> None:
    cfg = ConnectionConfigSync(protocol="http", transport=_SseTransport())
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapterSync(cfg, endpoint)

    execution = adapter.run("exit 7")
    assert execution.id == "exec-2"
    assert execution.error is not None
    assert execution.error.value == "7"
    assert execution.complete is None
    assert execution.exit_code == 7


def test_sync_run_command_streaming_tolerates_null_traceback() -> None:
    cfg = ConnectionConfigSync(protocol="http", transport=_SseTransport())
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapterSync(cfg, endpoint)

    execution = adapter.run("exit null")

    assert execution.id == "exec-null"
    assert execution.error is not None
    assert execution.error.value == "fork/exec /usr/bin/bash: resource temporarily unavailable"
    assert execution.error.traceback == []
    assert execution.complete is None


def test_sync_run_in_session_streaming_uses_generated_fields_and_exit_code() -> None:
    transport = _SseTransport()
    cfg = ConnectionConfigSync(protocol="http", transport=transport)
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapterSync(cfg, endpoint)

    execution = adapter.run_in_session(
        "sess-1",
        "pwd",
        working_directory="/var",
        timeout=timedelta(seconds=5),
    )

    assert execution.logs.stdout[0].text == "/var"
    assert execution.complete is not None
    assert execution.complete.execution_time_in_millis == 3
    assert execution.exit_code == 0


def test_sync_run_in_session_non_zero_exit_updates_exit_code() -> None:
    cfg = ConnectionConfigSync(protocol="http", transport=_SseTransport())
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapterSync(cfg, endpoint)

    execution = adapter.run_in_session("sess-2", "exit 7")

    assert execution.id == "sess-exec-2"
    assert execution.error is not None
    assert execution.error.value == "7"
    assert execution.complete is None
    assert execution.exit_code == 7


def test_sync_run_command_auto_resume_on_sse_disconnect() -> None:
    """SSE drops after first two stdout lines; resume replays the rest transparently."""
    transport = _SyncResumeTransport()
    cfg = ConnectionConfigSync(protocol="http", transport=transport)
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapterSync(cfg, endpoint)

    execution = adapter.run("echo lines")

    assert transport.post_count == 1, "should send POST /command"
    assert transport.resume_count == 1, "should send GET /command/:id/resume"
    assert transport.last_resume_eid == 2, "resume after_eid should match last received eid"

    assert execution.id == "cmd-resume-1"
    assert len(execution.logs.stdout) == 3
    assert execution.logs.stdout[0].text == "line1"
    assert execution.logs.stdout[1].text == "line2"
    assert execution.logs.stdout[2].text == "line3"
    assert execution.complete is not None
    assert execution.complete.execution_time_in_millis == 42
    assert execution.exit_code == 0
