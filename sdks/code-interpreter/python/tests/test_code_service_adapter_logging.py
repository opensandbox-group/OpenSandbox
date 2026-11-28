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

import logging

import httpx
import pytest
from opensandbox.config import ConnectionConfig, ConnectionConfigSync
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.sandboxes import SandboxEndpoint

from code_interpreter.adapters.code_adapter import CodesAdapter
from code_interpreter.sync.adapters.code_adapter import CodesAdapterSync


@pytest.mark.asyncio
@pytest.mark.parametrize("asynchronous", [False, True], ids=["sync", "async"])
@pytest.mark.parametrize(
    ("method", "argument"),
    [
        ("create_context", "python"),
        ("get_context", "expired-context"),
        ("list_contexts", "python"),
        ("delete_context", "expired-context"),
        ("delete_contexts", "python"),
        ("run", "print('hello')"),
        ("interrupt", "expired-execution"),
    ],
)
async def test_propagated_failures_are_logged_once_at_debug(
    caplog: pytest.LogCaptureFixture, asynchronous: bool, method: str, argument: str
) -> None:
    endpoint = SandboxEndpoint(endpoint="execd.example", port=44772)

    def respond(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            404, json={"code": "NOT_FOUND", "message": "No such resource"}
        )

    with caplog.at_level(logging.DEBUG, logger="code_interpreter"):
        with pytest.raises(SandboxApiException) as exc_info:
            if asynchronous:
                async with httpx.MockTransport(respond) as transport:
                    adapter = CodesAdapter(
                        endpoint, ConnectionConfig(transport=transport)
                    )
                    await getattr(adapter, method)(argument)
            else:
                with httpx.MockTransport(respond) as transport:
                    adapter_sync = CodesAdapterSync(
                        endpoint, ConnectionConfigSync(transport=transport)
                    )
                    getattr(adapter_sync, method)(argument)

    assert exc_info.value.status_code == 404
    assert exc_info.value.response_body is not None
    assert b"No such resource" in exc_info.value.response_body
    records = [
        record
        for record in caplog.records
        if record.name.startswith("code_interpreter.")
    ]
    assert [record.levelno for record in records] == [logging.DEBUG]
    assert records[0].exc_info is not None
