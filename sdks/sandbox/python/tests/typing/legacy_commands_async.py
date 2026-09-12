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
# Upstream command protocol (including argv), without recovery methods:
# adding required recovery methods must fail this fixture.
from datetime import timedelta
from typing import Protocol

from opensandbox.models.execd import (
    CommandLogs,
    CommandStatus,
    Execution,
    ExecutionHandlers,
    RunCommandOpts,
)
from opensandbox.services.command import Commands, get_execution_operations


class LegacyCommands(Protocol):
    async def run(
        self,
        command: str | list[str],
        *,
        opts: RunCommandOpts | None = None,
        handlers: ExecutionHandlers | None = None,
    ) -> Execution: ...

    async def interrupt(self, execution_id: str) -> None: ...

    async def get_command_status(self, execution_id: str) -> CommandStatus: ...

    async def get_background_command_logs(
        self, execution_id: str, cursor: int | None = None
    ) -> CommandLogs: ...

    async def create_session(self, *, working_directory: str | None = None) -> str: ...

    async def run_in_session(
        self,
        session_id: str,
        command: str,
        *,
        working_directory: str | None = None,
        timeout: timedelta | None = None,
        handlers: ExecutionHandlers | None = None,
    ) -> Execution: ...

    async def delete_session(self, session_id: str) -> None: ...


def accepts_existing_adapter(old: LegacyCommands) -> Commands:
    return old


def recovery_is_optional(old: LegacyCommands) -> None:
    get_execution_operations(old)
