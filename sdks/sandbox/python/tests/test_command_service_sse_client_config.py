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

from opensandbox.adapters.command_adapter import CommandsAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.transport import RetryAsyncTransport


def test_sse_client_has_event_stream_headers_and_no_read_timeout() -> None:
    cfg = ConnectionConfig(protocol="http")
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapter(cfg, endpoint)

    sse_client = adapter._sse_client
    assert sse_client is not None
    assert sse_client.headers.get("Accept") == "text/event-stream"
    assert sse_client.timeout.read is None


def test_sse_client_bypasses_retry_wrapper() -> None:
    """
    SSE bootstraps must not go through the retry wrapper: request bodies
    are not replayable, and a caller that opts into non-idempotent
    status retries would otherwise trigger duplicate execution.
    """
    cfg = ConnectionConfig(protocol="http").with_transport_if_missing()
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapter(cfg, endpoint)

    # Sanity: the normal client goes through the retry wrapper.
    assert isinstance(adapter._httpx_client._transport, RetryAsyncTransport)
    # SSE client's transport is the raw inner of the wrapper.
    assert not isinstance(adapter._sse_client._transport, RetryAsyncTransport)
    assert (
        adapter._sse_client._transport
        is adapter._httpx_client._transport.inner  # type: ignore[union-attr]
    )
