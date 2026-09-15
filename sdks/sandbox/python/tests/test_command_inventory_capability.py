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
from __future__ import annotations

from typing import Any, cast

from opensandbox.adapters.command_adapter import CommandsAdapter
from opensandbox.config import ConnectionConfig, ConnectionConfigSync
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sandbox import Sandbox
from opensandbox.services import CommandInventory, Commands
from opensandbox.sync.adapters.command_adapter import CommandsAdapterSync
from opensandbox.sync.sandbox import SandboxSync
from opensandbox.sync.services import CommandInventorySync, CommandsSync


class _Noop:
    pass


_NOOP = cast(Any, _Noop())


class _LegacyCommands:
    async def run(self, *args: Any, **kwargs: Any) -> Any:
        pass

    async def interrupt(self, *args: Any, **kwargs: Any) -> None:
        pass

    async def get_command_status(self, *args: Any, **kwargs: Any) -> Any:
        pass

    async def get_background_command_logs(self, *args: Any, **kwargs: Any) -> Any:
        pass

    async def create_session(self, *args: Any, **kwargs: Any) -> str:
        return "session"

    async def run_in_session(self, *args: Any, **kwargs: Any) -> Any:
        pass

    async def delete_session(self, *args: Any, **kwargs: Any) -> None:
        pass


class _LegacyCommandsSync:
    def run(self, *args: Any, **kwargs: Any) -> Any:
        pass

    def interrupt(self, *args: Any, **kwargs: Any) -> None:
        pass

    def get_command_status(self, *args: Any, **kwargs: Any) -> Any:
        pass

    def get_background_command_logs(self, *args: Any, **kwargs: Any) -> Any:
        pass

    def create_session(self, *args: Any, **kwargs: Any) -> str:
        return "session"

    def run_in_session(self, *args: Any, **kwargs: Any) -> Any:
        pass

    def delete_session(self, *args: Any, **kwargs: Any) -> None:
        pass


def test_legacy_command_services_have_no_inventory_capability() -> None:
    legacy_commands: Commands = _LegacyCommands()
    legacy_commands_sync: CommandsSync = _LegacyCommandsSync()

    sandbox = Sandbox(
        sandbox_id="sandbox-id",
        sandbox_service=_NOOP,
        filesystem_service=_NOOP,
        command_service=legacy_commands,
        health_service=_NOOP,
        metrics_service=_NOOP,
        egress_service=_NOOP,
        diagnostics_service=_NOOP,
        connection_config=ConnectionConfig(),
    )
    sandbox_sync = SandboxSync(
        sandbox_id="sandbox-id",
        sandbox_service=_NOOP,
        filesystem_service=_NOOP,
        command_service=legacy_commands_sync,
        health_service=_NOOP,
        metrics_service=_NOOP,
        egress_service=_NOOP,
        diagnostics_service=_NOOP,
        connection_config=ConnectionConfigSync(),
    )

    assert sandbox.command_inventory is None
    assert sandbox_sync.command_inventory is None


def test_command_adapters_expose_inventory_capability() -> None:
    endpoint = SandboxEndpoint(endpoint="localhost:8080")
    sandbox = Sandbox(
        sandbox_id="sandbox-id",
        sandbox_service=_NOOP,
        filesystem_service=_NOOP,
        command_service=CommandsAdapter(ConnectionConfig(), endpoint),
        health_service=_NOOP,
        metrics_service=_NOOP,
        egress_service=_NOOP,
        diagnostics_service=_NOOP,
        connection_config=ConnectionConfig(),
    )
    sandbox_sync = SandboxSync(
        sandbox_id="sandbox-id",
        sandbox_service=_NOOP,
        filesystem_service=_NOOP,
        command_service=CommandsAdapterSync(ConnectionConfigSync(), endpoint),
        health_service=_NOOP,
        metrics_service=_NOOP,
        egress_service=_NOOP,
        diagnostics_service=_NOOP,
        connection_config=ConnectionConfigSync(),
    )

    inventory: CommandInventory | None = sandbox.command_inventory
    inventory_sync: CommandInventorySync | None = sandbox_sync.command_inventory
    assert inventory is not None
    assert inventory_sync is not None
    assert callable(inventory.list_commands)
    assert callable(inventory_sync.list_commands)
