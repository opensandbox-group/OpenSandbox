#
# Copyright 2025 The OpenSandbox Authors
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
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import (
    InvalidArgumentException,
    SandboxApiException,
    SandboxRateLimitException,
)
from opensandbox.models.sandboxes import SandboxEndpoint

from code_interpreter.adapters.code_adapter import CodesAdapter
from code_interpreter.adapters.converter.code_execution_converter import (
    CodeExecutionConverter,
)
from code_interpreter.models.code import CodeContext, SupportedLanguage
from code_interpreter.sync.adapters.code_adapter import CodesAdapterSync


@pytest.fixture(
    params=[
        pytest.param((["Loading", "...done\n"], "Loading...done"), id="partial-line"),
        pytest.param(([".", ".", "."], "..."), id="progress-dots"),
        pytest.param((["first\n", "second\n"], "first\nsecond"), id="terminated-lines"),
        pytest.param((["first\n", "\n", "last\n"], "first\n\nlast"), id="blank-line"),
        pytest.param((["\n"], ""), id="only-blank-line"),
        pytest.param((["first\r", "last\r"], "first\nlast"), id="carriage-returns"),
        pytest.param((["first\r\n", "last\r\n"], "first\nlast"), id="crlf-lines"),
        pytest.param((["first\r", "\n", "last\r", "\n"], "first\nlast"), id="split-crlf"),
        pytest.param((["first\n", "last"], "first\nlast"), id="unterminated-final-fragment"),
        pytest.param((["", "Loading", "", "...done\n", ""], "Loading...done"), id="empty-fragments"),
        pytest.param(([], ""), id="no-output"),
    ]
)
def stream_fragments(request: pytest.FixtureRequest) -> tuple[list[str], str]:
    return request.param


def _fragment_response(
    request: httpx.Request, stream: str, fragments: list[str]
) -> httpx.Response:
    """Replay synthetic stream events, without requiring execd or Jupyter."""
    assert request.url.path == "/code"
    events = [
        {"type": "init", "text": "exec-fragments", "timestamp": 0},
        *[
            {"type": stream, "text": text, "timestamp": index}
            for index, text in enumerate(fragments, start=1)
        ],
        {"type": "execution_complete", "timestamp": len(fragments) + 1},
    ]
    return httpx.Response(
        200,
        headers={"Content-Type": "text/event-stream"},
        content="".join(f"data: {json.dumps(event)}\n\n" for event in events),
        request=request,
    )


@pytest.mark.asyncio
@pytest.mark.parametrize("stream", ["stdout", "stderr"])
async def test_run_code_preserves_stream_fragments(
    stream: str, stream_fragments: tuple[list[str], str]
) -> None:
    fragments, expected = stream_fragments
    transport = httpx.MockTransport(
        lambda request: _fragment_response(request, stream, fragments)
    )
    cfg = ConnectionConfig(protocol="http", transport=transport)
    adapter = CodesAdapter(SandboxEndpoint(endpoint="localhost:44772"), cfg)

    execution = await adapter.run("print('synthetic stream')")

    assert execution.id == "exec-fragments"
    assert execution.complete is not None
    assert [message.text for message in getattr(execution.logs, stream)] == fragments
    if stream == "stdout":
        assert execution.text == expected
        assert str(execution) == expected
    else:
        assert execution.text == ""
        assert str(execution) == (f"[stderr]\n{expected}" if fragments else "")


@pytest.mark.parametrize("stream", ["stdout", "stderr"])
def test_sync_run_code_preserves_stream_fragments(
    stream: str, stream_fragments: tuple[list[str], str]
) -> None:
    fragments, expected = stream_fragments
    transport = httpx.MockTransport(
        lambda request: _fragment_response(request, stream, fragments)
    )
    cfg = ConnectionConfigSync(protocol="http", transport=transport)
    adapter = CodesAdapterSync(SandboxEndpoint(endpoint="localhost:44772"), cfg)

    execution = adapter.run("print('synthetic stream')")

    assert execution.id == "exec-fragments"
    assert execution.complete is not None
    assert [message.text for message in getattr(execution.logs, stream)] == fragments
    if stream == "stdout":
        assert execution.text == expected
        assert str(execution) == expected
    else:
        assert execution.text == ""
        assert str(execution) == (f"[stderr]\n{expected}" if fragments else "")


class _SseTransport(httpx.AsyncBaseTransport):
    def __init__(self) -> None:
        self.last_request: httpx.Request | None = None

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        self.last_request = request
        body = (
            request.content.decode("utf-8")
            if isinstance(request.content, (bytes, bytearray))
            else ""
        )
        payload = json.loads(body) if body else {}

        if request.url.path == "/code" and payload.get("code") == "print(1)":
            sse = (
                b'data: {"type":"init","text":"exec-1","timestamp":1}\n\n'
                b'data: {"type":"stdout","text":"1\\n","timestamp":2}\n\n'
                b'data: {"type":"execution_complete","timestamp":3,"execution_time":7}\n\n'
            )
            return httpx.Response(
                200,
                headers={"Content-Type": "text/event-stream"},
                content=sse,
                request=request,
            )

        if request.url.path == "/code" and payload.get("code") == "print(2)":
            assert payload["context"]["language"] == "go"
            sse = (
                b'data: {"type":"init","text":"exec-2","timestamp":1}\n\n'
                b'data: {"type":"stdout","text":"2\\n","timestamp":2}\n\n'
                b'data: {"type":"execution_complete","timestamp":3,"execution_time":7}\n\n'
            )
            return httpx.Response(
                200,
                headers={"Content-Type": "text/event-stream"},
                content=sse,
                request=request,
            )

        return httpx.Response(
            400,
            headers={"x-request-id": "req-code-123"},
            content=b"bad",
            request=request,
        )


def test_code_execution_converter_includes_context() -> None:
    ctx = CodeContext(id="c1", language=SupportedLanguage.PYTHON)
    d = CodeExecutionConverter.to_api_run_code_request("print(1)", ctx)
    assert d["code"] == "print(1)"
    assert d["context"]["id"] == "c1"
    assert d["context"]["language"] == "python"


@pytest.mark.asyncio
async def test_run_code_streaming_happy_path_updates_execution() -> None:
    cfg = ConnectionConfig(protocol="http", transport=_SseTransport())
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CodesAdapter(endpoint, cfg)

    execution = await adapter.run("print(1)")
    assert execution.id == "exec-1"
    assert execution.logs.stdout[0].text == "1\n"


@pytest.mark.asyncio
async def test_run_code_streaming_merges_endpoint_headers() -> None:
    transport = _SseTransport()
    cfg = ConnectionConfig(
        protocol="http",
        transport=transport,
        headers={"X-Base": "base", "X-Shared": "base"},
    )
    endpoint = SandboxEndpoint(
        endpoint="localhost:44772",
        port=44772,
        headers={"X-Endpoint": "endpoint", "X-Shared": "endpoint"},
    )
    adapter = CodesAdapter(endpoint, cfg)

    execution = await adapter.run("print(1)")

    assert execution.id == "exec-1"
    assert transport.last_request is not None
    assert transport.last_request.headers["X-Base"] == "base"
    assert transport.last_request.headers["X-Endpoint"] == "endpoint"
    assert transport.last_request.headers["X-Shared"] == "endpoint"


@pytest.mark.asyncio
async def test_run_code_can_accept_language_string_without_context() -> None:
    cfg = ConnectionConfig(protocol="http", transport=_SseTransport())
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CodesAdapter(endpoint, cfg)

    execution = await adapter.run("print(2)", language=SupportedLanguage.GO)
    assert execution.id == "exec-2"
    assert execution.logs.stdout[0].text == "2\n"


@pytest.mark.asyncio
async def test_run_code_rejects_blank_code() -> None:
    cfg = ConnectionConfig(protocol="http")
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CodesAdapter(endpoint, cfg)

    with pytest.raises(InvalidArgumentException):
        await adapter.run("   ")


@pytest.mark.asyncio
async def test_run_code_rejects_mismatched_language_and_context() -> None:
    cfg = ConnectionConfig(protocol="http", transport=_SseTransport())
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CodesAdapter(endpoint, cfg)

    with pytest.raises(InvalidArgumentException):
        await adapter.run(
            "print(1)",
            context=CodeContext(language=SupportedLanguage.PYTHON),
            language=SupportedLanguage.GO,
        )


@pytest.mark.asyncio
async def test_run_code_non_200_raises_api_exception() -> None:
    cfg = ConnectionConfig(protocol="http", transport=_SseTransport())
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CodesAdapter(endpoint, cfg)

    with pytest.raises(SandboxApiException) as ei:
        await adapter.run("other")
    assert ei.value.request_id == "req-code-123"
    # The server's error body is spliced into the message so logs carry the reason.
    assert "bad" in str(ei.value)


@pytest.mark.asyncio
async def test_run_code_non_200_keeps_structured_error_and_rate_limit() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            429,
            headers={"Retry-After": "3"},
            json={"code": "RATE_LIMITED", "message": "slow down"},
            request=request,
        )

    cfg = ConnectionConfig(protocol="http", transport=httpx.MockTransport(handler))
    adapter = CodesAdapter(SandboxEndpoint(endpoint="localhost:44772"), cfg)

    with pytest.raises(SandboxRateLimitException) as ei:
        await adapter.run("print(1)")
    assert ei.value.error.code == "RATE_LIMITED"
    assert ei.value.error.message == "slow down"
    assert ei.value.retry_after == timedelta(seconds=3)
    assert ei.value.response_body is not None
