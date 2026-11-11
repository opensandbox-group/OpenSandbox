#
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
#
"""Tests for CommandsAdapter.set_env (runtime env variable injection)."""

from __future__ import annotations

import json
import os
import subprocess
import tempfile

import httpx
import pytest

from opensandbox.adapters.command_adapter import CommandsAdapter
from opensandbox.adapters.converter.env_entry import build_set_env_command
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import (
    InvalidArgumentException,
    SandboxApiException,
    SandboxException,
)
from opensandbox.models.sandboxes import SandboxEndpoint

_SUCCESS_SSE = (
    b'data: {"type":"execution_complete","timestamp":1,"execution_time":1}\n\n'
)

# Hand-written golden literal (not derived from the implementation).
_GOLDEN_SIMPLE_COMMAND = (
    "if [ -z \"${EXECD_ENVS:-}\" ]; then "
    "printf '%s\\n' "
    "'EXECD_ENVS is not set; cannot persist environment variable MY_TOKEN' "
    ">&2; exit 1; fi\n"
    'mkdir -p "$(dirname "$EXECD_ENVS")"\n'
    "printf '%s\\n' 'MY_TOKEN='\\''value'\\''' >> \"$EXECD_ENVS\""
)

# Round-trip cases: (key, value, the exact line the snippet must append).
_ROUND_TRIP_CASES = [
    ("MY_TOKEN", "value", "MY_TOKEN='value'"),
    ("MY_VAR", "line1\nline2 $HOME \\path", "MY_VAR='line1\nline2 $HOME \\path'"),
    ("KV", "a=b=c", "KV='a=b=c'"),
    ("EMPTY", "", "EMPTY=''"),
    ("GREETING", "it's fine \"quoted\"\ttab", 'GREETING="it\'s fine \\"quoted\\"\\ttab"'),
    ("PATHY", "it's\nC:\\path", 'PATHY="it\'s\\nC:\\\\path"'),
]


class _CaptureTransport(httpx.AsyncBaseTransport):
    def __init__(self, sse: bytes = _SUCCESS_SSE) -> None:
        self.sse = sse
        self.requests: list[dict] = []

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        body = (
            request.content.decode("utf-8")
            if isinstance(request.content, (bytes, bytearray))
            else ""
        )
        self.requests.append(json.loads(body) if body else {})
        return httpx.Response(
            200,
            headers={"Content-Type": "text/event-stream"},
            content=self.sse,
            request=request,
        )


def _make_adapter(transport: httpx.AsyncBaseTransport) -> CommandsAdapter:
    cfg = ConnectionConfig(protocol="http", transport=transport)
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    return CommandsAdapter(cfg, endpoint)


@pytest.mark.asyncio
async def test_set_env_appends_entry_via_env_file() -> None:
    transport = _CaptureTransport()
    adapter = _make_adapter(transport)

    await adapter.set_env("MY_TOKEN", "value")

    assert len(transport.requests) == 1
    assert transport.requests[0] == {"command": _GOLDEN_SIMPLE_COMMAND}


@pytest.mark.asyncio
@pytest.mark.parametrize("key,value,expected_line", _ROUND_TRIP_CASES)
async def test_set_env_snippet_appends_well_formed_line(
    key: str, value: str, expected_line: str
) -> None:
    """Run the emitted snippet through /bin/sh and verify the appended file
    line byte-for-byte (guards against printf format/argument mismatches)."""
    transport = _CaptureTransport()
    adapter = _make_adapter(transport)

    await adapter.set_env(key, value)
    command = transport.requests[0]["command"]

    with tempfile.TemporaryDirectory() as tmp:
        env_file = os.path.join(tmp, ".env")
        subprocess.run(
            ["/bin/sh", "-c", command],
            check=True,
            env={"PATH": os.environ.get("PATH", ""), "EXECD_ENVS": env_file},
        )
        with open(env_file, encoding="utf-8") as fh:
            assert fh.read() == expected_line + "\n"


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "key", ["", "1ABC", "MY-TOKEN", "MY TOKEN", "A=B", "A.B", "A\n"]
)
async def test_set_env_rejects_invalid_keys(key: str) -> None:
    transport = _CaptureTransport()
    adapter = _make_adapter(transport)

    with pytest.raises(InvalidArgumentException, match="set_env key must match"):
        await adapter.set_env(key, "value")
    assert transport.requests == []


def test_build_set_env_command_rejects_nul_bytes() -> None:
    with pytest.raises(InvalidArgumentException, match="NUL"):
        build_set_env_command("MY_TOKEN", "a\0b")


@pytest.mark.asyncio
async def test_set_env_raises_with_stderr_when_append_fails() -> None:
    failure_sse = (
        b'data: {"type":"init","text":"exec-1","timestamp":1}\n\n'
        b'data: {"type":"stderr","text":"EXECD_ENVS is not set; cannot persist environment variable MY_TOKEN","timestamp":2}\n\n'
        b'data: {"type":"error","error":{"ename":"CommandExecError","evalue":"1","traceback":["exit status 1"]},"timestamp":3}\n\n'
    )
    adapter = _make_adapter(_CaptureTransport(failure_sse))

    with pytest.raises(SandboxException, match="EXECD_ENVS is not set"):
        await adapter.set_env("MY_TOKEN", "value")


@pytest.mark.asyncio
async def test_set_env_treats_dropped_stream_without_completion_as_failure() -> None:
    incomplete_sse = b'data: {"type":"init","text":"exec-1","timestamp":1}\n\n'
    adapter = _make_adapter(_CaptureTransport(incomplete_sse))

    with pytest.raises(SandboxException, match="set_env failed"):
        await adapter.set_env("MY_TOKEN", "value")


@pytest.mark.asyncio
async def test_set_env_wraps_transport_errors_as_sandbox_exception() -> None:
    class _BoomTransport(httpx.AsyncBaseTransport):
        async def handle_async_request(
            self, request: httpx.Request
        ) -> httpx.Response:
            return httpx.Response(500, content=b"boom", request=request)

    adapter = _make_adapter(_BoomTransport())

    with pytest.raises(SandboxApiException):
        await adapter.set_env("MY_TOKEN", "value")
