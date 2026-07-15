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
import httpx
import pytest

from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import InvalidArgumentException
from opensandbox.models.sandboxes import NetworkPolicy, NetworkRule
from opensandbox.sandbox import Sandbox


class _NoopService:
    pass


class _NoopEgressService:
    async def get_policy(self) -> NetworkPolicy:  # pragma: no cover
        return NetworkPolicy(
            defaultAction="deny",
            egress=[NetworkRule(action="allow", target="pypi.org")],
        )

    async def patch_rules(self, rules: list[NetworkRule]) -> None:  # pragma: no cover
        return None


@pytest.mark.asyncio
async def test_sandbox_close_does_not_close_user_transport() -> None:
    class CustomTransport(httpx.AsyncBaseTransport):
        def __init__(self) -> None:
            self.closed = False

        async def handle_async_request(self, request: httpx.Request) -> httpx.Response:  # pragma: no cover
            raise RuntimeError("not used")

        async def aclose(self) -> None:
            self.closed = True

    t = CustomTransport()
    cfg = ConnectionConfig(transport=t)

    sbx = Sandbox(
        sandbox_id=str(__import__("uuid").uuid4()),
        sandbox_service=_NoopService(),
        filesystem_service=_NoopService(),
        command_service=_NoopService(),
        health_service=_NoopService(),
        metrics_service=_NoopService(),
        egress_service=_NoopEgressService(),
        diagnostics_service=_NoopService(),
        connection_config=cfg,
        custom_health_check=None,
    )

    await sbx.close()
    assert t.closed is False


@pytest.mark.asyncio
async def test_sandbox_connect_requires_id() -> None:
    with pytest.raises(InvalidArgumentException):
        await Sandbox.connect(sandbox_id="", connection_config=ConnectionConfig())
