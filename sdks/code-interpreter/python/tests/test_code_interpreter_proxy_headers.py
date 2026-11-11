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

import json

import httpx
import pytest
from opensandbox import Sandbox, SandboxSync
from opensandbox.config import ConnectionConfig, ConnectionConfigSync

from code_interpreter import CodeInterpreter, CodeInterpreterSync

API_KEY = "test-tenant-key"


@pytest.fixture(params=[True, False], ids=["proxy", "direct"])
def use_server_proxy(request: pytest.FixtureRequest) -> bool:
    return request.param


@pytest.fixture(params=["argument", "environment"])
def api_key(
    request: pytest.FixtureRequest, monkeypatch: pytest.MonkeyPatch
) -> str | None:
    if request.param == "environment":
        monkeypatch.setenv("OPEN_SANDBOX_API_KEY", API_KEY)
        return None
    monkeypatch.delenv("OPEN_SANDBOX_API_KEY", raising=False)
    return API_KEY


@pytest.fixture
def transport(use_server_proxy: bool) -> httpx.MockTransport:
    def handle(request: httpx.Request) -> httpx.Response:
        if "/endpoints/" in request.url.path:
            assert request.headers["OPEN-SANDBOX-API-KEY"] == API_KEY
            return httpx.Response(
                200,
                json={
                    "endpoint": "sandbox.test/execd",
                    "headers": {"X-Endpoint": "endpoint", "X-Shared": "endpoint"},
                },
            )

        assert request.headers.get("OPEN-SANDBOX-API-KEY") == (
            API_KEY if use_server_proxy else None
        )
        assert request.headers["User-Agent"] == "custom-agent"
        assert request.headers["X-Base"] == "base"
        assert request.headers["X-Endpoint"] == "endpoint"
        assert request.headers["X-Shared"] == "endpoint"
        if request.url.path == "/execd/code/context":
            return httpx.Response(200, json={"id": "ctx-1", "language": "python"})
        assert request.url.path == "/execd/code"
        assert request.headers["Accept"] == "text/event-stream"
        events = [
            {"type": "stdout", "timestamp": 1, "text": "hello\n"},
            {"type": "execution_complete", "timestamp": 2, "execution_time_in_millis": 1},
        ]
        return httpx.Response(
            200,
            headers={"Content-Type": "text/event-stream"},
            content="".join(f"data: {json.dumps(event)}\n\n" for event in events),
        )

    return httpx.MockTransport(handle)


@pytest.fixture
def connection_headers() -> dict[str, str]:
    return {"User-Agent": "custom-agent", "X-Base": "base", "X-Shared": "base"}


@pytest.mark.asyncio
@pytest.mark.parametrize("operation", ["context", "run"])
async def test_async_code_requests_use_sandbox_proxy_headers(
    use_server_proxy: bool,
    api_key: str | None,
    transport: httpx.MockTransport,
    connection_headers: dict[str, str],
    operation: str,
) -> None:
    config = ConnectionConfig(
        domain="sandbox.test",
        protocol="http",
        api_key=api_key,
        use_server_proxy=use_server_proxy,
        headers=connection_headers,
        transport=transport,
    )
    sandbox = await Sandbox.connect(
        "sandbox-1", connection_config=config, skip_health_check=True
    )
    interpreter = await CodeInterpreter.create(sandbox, skip_health_check=True)
    try:
        if operation == "context":
            context = await interpreter.codes.create_context("python")
            assert context.id == "ctx-1"
        else:
            execution = await interpreter.codes.run("print('hello')")
            assert execution.text == "hello"
    finally:
        await interpreter.aclose()
        await sandbox.close()


@pytest.mark.parametrize("operation", ["context", "run"])
def test_sync_code_requests_use_sandbox_proxy_headers(
    use_server_proxy: bool,
    api_key: str | None,
    transport: httpx.MockTransport,
    connection_headers: dict[str, str],
    operation: str,
) -> None:
    config = ConnectionConfigSync(
        domain="sandbox.test",
        protocol="http",
        api_key=api_key,
        use_server_proxy=use_server_proxy,
        headers=connection_headers,
        transport=transport,
    )
    sandbox = SandboxSync.connect(
        "sandbox-1", connection_config=config, skip_health_check=True
    )
    interpreter = CodeInterpreterSync.create(sandbox, skip_health_check=True)
    try:
        if operation == "context":
            context = interpreter.codes.create_context("python")
            assert context.id == "ctx-1"
        else:
            execution = interpreter.codes.run("print('hello')")
            assert execution.text == "hello"
    finally:
        interpreter.close()
        sandbox.close()
