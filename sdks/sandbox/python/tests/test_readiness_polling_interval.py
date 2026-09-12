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
"""Negative readiness polling intervals are rejected before any request."""

from datetime import timedelta

import httpx
import pytest

from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import InvalidArgumentException, SandboxApiException
from opensandbox.internal.readiness import ReadinessBudget
from opensandbox.sandbox import Sandbox
from opensandbox.sync.sandbox import SandboxSync

INTERVALS = [timedelta(milliseconds=-1), timedelta(microseconds=-1)]


def _recording_transport(calls: list[str]) -> httpx.MockTransport:
    def handle(request: httpx.Request) -> httpx.Response:
        calls.append(f"{request.method} {request.url.path}")
        if request.url.path == "/ping":
            return httpx.Response(503)
        return httpx.Response(
            200, json={"id": "sb", "endpoint": "localhost:44772", "headers": {}}
        )

    return httpx.MockTransport(handle)


@pytest.mark.parametrize("interval", INTERVALS)
def test_budget_rejects_negative_interval(interval: timedelta) -> None:
    with pytest.raises(InvalidArgumentException):
        ReadinessBudget(timedelta(seconds=1), interval)


@pytest.mark.parametrize("interval", INTERVALS)
@pytest.mark.parametrize("method", ["create", "connect", "resume"])
@pytest.mark.asyncio
async def test_async_rejects_negative_interval_before_requests(
    method: str, interval: timedelta
) -> None:
    calls: list[str] = []
    config = ConnectionConfig(
        domain="localhost:8080", transport=_recording_transport(calls)
    )
    options = {
        "connection_config": config,
        "health_check_polling_interval": interval,
    }
    with pytest.raises(InvalidArgumentException):
        if method == "create":
            await Sandbox.create(
                "python:3.11", ready_timeout=timedelta(seconds=1), **options
            )
        elif method == "connect":
            await Sandbox.connect(
                "sb", connect_timeout=timedelta(seconds=1), **options
            )
        else:
            await Sandbox.resume("sb", resume_timeout=timedelta(seconds=1), **options)
    assert calls == []


@pytest.mark.parametrize("interval", INTERVALS)
@pytest.mark.parametrize("method", ["create", "connect", "resume"])
def test_sync_rejects_negative_interval_before_requests(
    method: str, interval: timedelta
) -> None:
    calls: list[str] = []
    config = ConnectionConfigSync(
        domain="localhost:8080", transport=_recording_transport(calls)
    )
    options = {
        "connection_config": config,
        "health_check_polling_interval": interval,
    }
    with pytest.raises(InvalidArgumentException):
        if method == "create":
            SandboxSync.create(
                "python:3.11", ready_timeout=timedelta(seconds=1), **options
            )
        elif method == "connect":
            SandboxSync.connect("sb", connect_timeout=timedelta(seconds=1), **options)
        else:
            SandboxSync.resume("sb", resume_timeout=timedelta(seconds=1), **options)
    assert calls == []


@pytest.mark.parametrize("interval", INTERVALS)
def test_create_skipping_health_check_does_not_validate_interval(
    interval: timedelta,
) -> None:
    calls: list[str] = []
    config = ConnectionConfigSync(
        domain="localhost:8080", transport=_recording_transport(calls)
    )
    # The mock create response is not a valid sandbox; reaching the server is enough.
    with pytest.raises(SandboxApiException):
        SandboxSync.create(
            "python:3.11",
            connection_config=config,
            health_check_polling_interval=interval,
            skip_health_check=True,
        )
    assert calls[0].startswith("POST ")
