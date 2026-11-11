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
"""
Command service interface.

Protocol for sandbox command execution operations.
"""

from datetime import timedelta
from typing import Protocol

from opensandbox.models.execd import (
    CommandLogs,
    CommandStatus,
    Execution,
    ExecutionHandlers,
    RunCommandOpts,
)


class Commands(Protocol):
    """
    Command execution service for sandbox environments.

    This service provides secure command execution capabilities within sandbox
    environments, with support for streaming output, timeout handling, and
    session management.
    """

    async def run(
        self,
        command: str | list[str],
        *,
        opts: RunCommandOpts | None = None,
        handlers: ExecutionHandlers | None = None,
    ) -> Execution:
        """
        Execute shell text or native executable arguments in the sandbox.

        The command can be executed in foreground (streaming) or background mode
        based on the request configuration.

        Args:
            command: Shell text or a native argv list
            opts: Command execution options (e.g. background, working_directory)
            handlers: Optional async handlers for streaming events (stdout/stderr/result/init/complete/error)

        Returns:
            An Execution handle representing the running command instance

        Raises:
            SandboxException: if the operation fails
        """
        ...

    async def set_env(self, key: str, value: str) -> None:
        """
        Persist an environment variable for future commands and sessions.

        Appends ``KEY=VALUE`` to the sandbox env file that the runtime loads for
        every command and session (the file pointed to by the sandbox's
        ``EXECD_ENVS`` variable, resolved inside the sandbox). Keys must match
        ``[A-Za-z_][A-Za-z0-9_]*``. Values without a single quote are stored
        verbatim; values containing a single quote use the env file's
        double-quoted form, in which shell-style ``$NAME`` sequences may be
        expanded when the runtime loads the file. The file is append-only: when
        a key is written multiple times, the last entry wins.

        Args:
            key: Environment variable name
            value: Environment variable value

        Raises:
            InvalidArgumentException: if key or value is invalid
            SandboxException: if the sandbox fails to persist the variable
        """
        ...

    async def interrupt(self, execution_id: str) -> None:
        """
        Interrupt and terminate a running command execution.

        This sends a termination signal (usually SIGTERM/SIGKILL) to the process
        associated with the given execution ID.

        Args:
            execution_id: Unique identifier of the execution to interrupt

        Raises:
            SandboxException: if the operation fails
        """
        ...

    async def get_command_status(self, execution_id: str) -> CommandStatus:
        """
        Get the current running status for a command.

        Args:
            execution_id: Unique identifier of the execution to query

        Returns:
            CommandStatus describing running state and exit code if available

        Raises:
            SandboxException: if the operation fails
        """
        ...

    async def get_background_command_logs(
        self, execution_id: str, cursor: int | None = None
    ) -> CommandLogs:
        """
        Get background command logs (non-streamed).

        Args:
            execution_id: Unique identifier of the execution to query
            cursor: Optional line cursor for incremental reads

        Returns:
            CommandLogs containing raw output and latest cursor

        Raises:
            SandboxException: if the operation fails
        """
        ...

    async def create_session(self, *, working_directory: str | None = None) -> str:
        """
        Create a bash session. Returns session_id for run_in_session and delete_session.

        Args:
            working_directory: Optional working directory for the session.

        Returns:
            Session ID string.

        Raises:
            SandboxException: if the operation fails.
        """
        ...

    async def run_in_session(
        self,
        session_id: str,
        command: str,
        *,
        working_directory: str | None = None,
        timeout: timedelta | None = None,
        handlers: ExecutionHandlers | None = None,
    ) -> Execution:
        """
        Run a shell command in an existing bash session (streams output via SSE).

        Args:
            session_id: Session ID from create_session.
            command: Shell command to execute.
            working_directory: Optional working directory override for this run.
            timeout: Optional max execution time for this session run.
            handlers: Optional async handlers for streaming events.

        Returns:
            Execution handle with logs and results.

        Raises:
            SandboxException: if the operation fails.
        """
        ...

    async def delete_session(self, session_id: str) -> None:
        """
        Delete a bash session and release resources.

        Args:
            session_id: Session ID to delete.

        Raises:
            SandboxException: if the operation fails.
        """
        ...
