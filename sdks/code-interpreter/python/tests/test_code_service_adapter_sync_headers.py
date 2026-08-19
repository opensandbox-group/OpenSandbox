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
import pytest
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.sandboxes import SandboxEndpoint

from code_interpreter.sync.adapters.code_adapter import CodesAdapterSync


def test_sync_adapter_non_200_includes_error_body_in_message() -> None:
    import httpx

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            400,
            headers={"x-request-id": "req-code-sync-123"},
            content=b"bad request body",
            request=request,
        )

    cfg = ConnectionConfigSync(protocol="http", transport=httpx.MockTransport(handler))
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CodesAdapterSync(endpoint, cfg)

    with pytest.raises(SandboxApiException) as ei:
        adapter.run("other")
    assert ei.value.request_id == "req-code-sync-123"
    # The server's error body is spliced into the message so logs carry the reason.
    assert "bad request body" in str(ei.value)


def test_sync_adapter_merges_endpoint_headers_into_both_clients() -> None:
    cfg = ConnectionConfigSync(protocol="http", headers={"X-Base": "base"})
    endpoint = SandboxEndpoint(
        endpoint="localhost:44772",
        headers={"X-Endpoint": "endpoint"},
    )

    adapter = CodesAdapterSync(endpoint, cfg)

    assert adapter._httpx_client.headers["X-Base"] == "base"
    assert adapter._httpx_client.headers["X-Endpoint"] == "endpoint"
    assert adapter._sse_client.headers["X-Base"] == "base"
    assert adapter._sse_client.headers["X-Endpoint"] == "endpoint"


def test_sync_adapter_endpoint_headers_override_connection_headers() -> None:
    cfg = ConnectionConfigSync(protocol="http", headers={"X-Shared": "base"})
    endpoint = SandboxEndpoint(
        endpoint="localhost:44772",
        headers={"X-Shared": "endpoint"},
    )

    adapter = CodesAdapterSync(endpoint, cfg)

    assert adapter._httpx_client.headers["X-Shared"] == "endpoint"
    assert adapter._sse_client.headers["X-Shared"] == "endpoint"
