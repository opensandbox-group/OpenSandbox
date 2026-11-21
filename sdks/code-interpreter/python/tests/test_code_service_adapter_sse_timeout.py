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
"""Code runs stream SSE: the read timeout bounds silence, not the run."""

from datetime import timedelta

import pytest
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.sandboxes import SandboxEndpoint

from code_interpreter.adapters.code_adapter import CodesAdapter
from code_interpreter.sync.adapters.code_adapter import CodesAdapterSync


@pytest.mark.parametrize(
    ("request_timeout", "expected_read"),
    [(timedelta(seconds=5), 30.0), (timedelta(seconds=60), 60.0)],
)
def test_code_sse_read_timeout_is_request_timeout_with_a_floor(
    request_timeout: timedelta, expected_read: float
) -> None:
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    async_adapter = CodesAdapter(
        endpoint, ConnectionConfig(protocol="http", request_timeout=request_timeout)
    )
    sync_adapter = CodesAdapterSync(
        endpoint, ConnectionConfigSync(protocol="http", request_timeout=request_timeout)
    )

    for client in (async_adapter._sse_client, sync_adapter._sse_client):
        assert client.timeout.read == expected_read
        assert client.timeout.connect == request_timeout.total_seconds()
        assert client.timeout.write == request_timeout.total_seconds()
        assert client.timeout.pool is None
