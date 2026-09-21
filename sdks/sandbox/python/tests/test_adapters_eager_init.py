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
import pytest

from opensandbox.adapters.command_adapter import CommandsAdapter
from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.adapters.health_adapter import HealthAdapter
from opensandbox.adapters.metrics_adapter import MetricsAdapter
from opensandbox.adapters.sandboxes_adapter import SandboxesAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.models.sandboxes import SandboxEndpoint


def test_sandbox_service_adapter_eager_init() -> None:
    cfg = ConnectionConfig(domain="localhost:8080", api_key="x")
    adapter = SandboxesAdapter(cfg)
    assert adapter is not None


@pytest.mark.asyncio
async def test_execd_service_adapters_eager_init_and_urls() -> None:
    cfg = ConnectionConfig(protocol="http")
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)

    cmd = CommandsAdapter(cfg, endpoint)
    fs = FilesystemAdapter(cfg, endpoint)
    health = HealthAdapter(cfg, endpoint)
    metrics = MetricsAdapter(cfg, endpoint)

    assert cmd._get_execd_url("/ping").endswith("/ping")
    assert fs._get_execd_url("/files/download").endswith("/files/download")

    # Ensure openapi clients are available without lazy init
    assert await cmd._get_client() is not None
    assert await fs._get_client() is not None
    assert await health._get_client() is not None
    assert await metrics._get_client() is not None
