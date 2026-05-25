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
"""
Synchronous command adapter implementation (including SSE streaming).
"""

import json
import logging
from datetime import timedelta

import httpx

from opensandbox.adapters.converter.event_node import EventNode
from opensandbox.adapters.converter.exception_converter import (
    ExceptionConverter,
)
from opensandbox.adapters.converter.execution_converter import (
    ExecutionConverter,
)
from opensandbox.adapters.converter.response_handler import (
    extract_request_id,
    handle_api_error,
)
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import InvalidArgumentException, SandboxApiException
from opensandbox.models.execd import (
    CommandLogs,
    CommandStatus,
    Execution,
    RunCommandOpts,
)
from opensandbox.models.execd_sync import ExecutionHandlersSync
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.converter.execution_event_dispatcher import (
    ExecutionEventDispatcherSync,
)
from opensandbox.sync.services.command import CommandsSync

logger = logging.getLogger(__name__)


def _resolve_run_in_session_timeout(timeout: timedelta | None) -> int | None:
    if timeout is None:
        return None
    if isinstance(timeout, timedelta):
        timeout_ms = int(timeout.total_seconds() * 1000)
        if timeout_ms < 0:
            raise InvalidArgumentException("timeout must be positive")
        return timeout_ms
    raise InvalidArgumentException(
        "timeout must be a datetime.timedelta or None"
    )


def _infer_foreground_exit_code(execution: Execution) -> int | None:
    if execution.error is not None:
        try:
            return int(execution.error.value)
        except (TypeError, ValueError):
            return None
    if execution.complete is not None:
        return 0
    return None


def _build_run_command_request_body(command: str, opts: RunCommandOpts):
    return ExecutionConverter.to_api_run_command_request(command, opts)


def _build_run_in_session_request_body(
    command: str,
    working_directory: str | None,
    timeout: int | None,
):
    from opensandbox.api.execd.models.run_in_session_request import (
        RunInSessionRequest,
    )
    from opensandbox.api.execd.types import UNSET

    return RunInSessionRequest(
        command=command,
        cwd=working_directory if working_directory else UNSET,
        timeout=timeout if timeout is not None else UNSET,
    )


def _decode_sse_event_line(line: str) -> EventNode | None:
    if not line or not line.strip():
        return None

    if line.startswith((":", "event:", "id:", "retry:")):
        return None

    data = line[5:].strip() if line.startswith("data:") else line
    if not data:
        return None

    try:
        event_dict = json.loads(data)
        return EventNode(**event_dict)
    except Exception as e:
        logger.error("Failed to parse SSE line: %s", line, exc_info=e)
        return None


class CommandsAdapterSync(CommandsSync):
    """
    Synchronous implementation of :class:`~opensandbox.sync.services.command.CommandsSync`.

    This adapter wraps openapi-python-client generated clients for simple operations and
    uses direct ``httpx`` streaming for SSE (Server-Sent Events) command execution output.
    """

    RUN_COMMAND_PATH = "/command"
    SESSION_PATH = "/session"
    RUN_IN_SESSION_PATH = "/session/{session_id}/run"

    def __init__(self, connection_config: ConnectionConfigSync, execd_endpoint: SandboxEndpoint) -> None:
        """
        Initialize the command adapter (sync).

        Args:
            connection_config: Connection configuration (shared transport, headers, timeouts)
            execd_endpoint: Endpoint for execd service
        """
        self.connection_config = connection_config
        self.execd_endpoint = execd_endpoint

        from opensandbox.api.execd import Client

        base_url = f"{self.connection_config.protocol}://{self.execd_endpoint.endpoint}"
        timeout_seconds = self.connection_config.request_timeout.total_seconds()
        timeout = httpx.Timeout(timeout_seconds)

        headers = {
            "User-Agent": self.connection_config.user_agent,
            **self.connection_config.headers,
            **self.execd_endpoint.headers,
        }

        self._client = Client(base_url=base_url, timeout=timeout)

        self._httpx_client = httpx.Client(
            base_url=base_url,
            headers=headers,
            timeout=timeout,
            transport=self.connection_config.transport,
        )
        self._client.set_httpx_client(self._httpx_client)

        # SSE client (read timeout disabled); endpoint headers already in headers
        sse_headers = {
            **headers,
            "Accept": "text/event-stream",
            "Cache-Control": "no-cache",
        }
        self._sse_client = httpx.Client(
            headers=sse_headers,
            timeout=httpx.Timeout(
                connect=timeout_seconds,
                read=None,
                write=timeout_seconds,
                pool=None,
            ),
            transport=self.connection_config.transport,
        )

    def _get_execd_url(self, path: str) -> str:
        """Build URL for execd endpoint."""
        return f"{self.connection_config.protocol}://{self.execd_endpoint.endpoint}{path}"

    def _execute_streaming_request(
        self,
        *,
        url: str,
        json_body: dict,
        handlers: ExecutionHandlersSync | None,
        infer_exit_code: bool,
        failure_message: str,
    ) -> Execution:
        execution = Execution(id=None, execution_count=None, result=[], error=None)
        dispatcher = ExecutionEventDispatcherSync(execution, handlers)

        with self._sse_client.stream("POST", url, json=json_body) as response:
            if response.status_code != 200:
                response.read()
                raise SandboxApiException(
                    message=f"{failure_message}. Status code: {response.status_code}",
                    status_code=response.status_code,
                    request_id=extract_request_id(response.headers),
                )

            for line in response.iter_lines():
                event_node = _decode_sse_event_line(line)
                if event_node is None:
                    continue
                dispatcher.dispatch(event_node)

        if infer_exit_code:
            execution.exit_code = _infer_foreground_exit_code(execution)

        return execution

    def run(
        self,
        command: str,
        *,
        opts: RunCommandOpts | None = None,
        handlers: ExecutionHandlersSync | None = None,
    ) -> Execution:
        if not command.strip():
            raise InvalidArgumentException("Command cannot be empty")

        try:
            opts = opts or RunCommandOpts()
            json_body = _build_run_command_request_body(command, opts).to_dict()
            url = self._get_execd_url(self.RUN_COMMAND_PATH)
            return self._execute_streaming_request(
                url=url,
                json_body=json_body,
                handlers=handlers,
                infer_exit_code=not opts.background,
                failure_message="Failed to run command",
            )

        except Exception as e:
            logger.error("Failed to run command (length: %s)", len(command), exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def interrupt(self, execution_id: str) -> None:
        """
        Interrupt a running command execution.

        Args:
            execution_id: Execution id returned by execd for the running command
        """
        try:
            from opensandbox.api.execd.api.command import interrupt_command

            response_obj = interrupt_command.sync_detailed(client=self._client, id=execution_id)
            handle_api_error(response_obj, "Interrupt command")
        except Exception as e:
            logger.error("Failed to interrupt command", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def get_command_status(self, execution_id: str) -> CommandStatus:
        """Get the current running status for a command."""
        try:
            from opensandbox.adapters.converter.command_model_converter import (
                to_command_status,
            )
            from opensandbox.adapters.converter.response_handler import require_parsed
            from opensandbox.api.execd.api.command import get_command_status
            from opensandbox.api.execd.models import CommandStatusResponse

            response_obj = get_command_status.sync_detailed(
                client=self._client,
                id=execution_id,
            )
            handle_api_error(response_obj, "Get command status")
            parsed = require_parsed(response_obj, CommandStatusResponse, "Get command status")
            return to_command_status(parsed)
        except Exception as e:
            logger.error("Failed to get command status", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def get_background_command_logs(
        self, execution_id: str, cursor: int | None = None
    ) -> CommandLogs:
        """Get background command logs (non-streamed)."""
        try:
            from opensandbox.adapters.converter.response_handler import require_parsed
            from opensandbox.api.execd.api.command import get_background_command_logs
            from opensandbox.api.execd.types import UNSET

            response_obj = get_background_command_logs.sync_detailed(
                client=self._client,
                id=execution_id,
                cursor=cursor if cursor is not None else UNSET,
            )
            handle_api_error(response_obj, "Get command logs")
            content = require_parsed(response_obj, str, "Get command logs")
            cursor_header = response_obj.headers.get("EXECD-COMMANDS-TAIL-CURSOR")
            next_cursor = None
            if cursor_header:
                try:
                    next_cursor = int(cursor_header)
                except ValueError:
                    next_cursor = None
            return CommandLogs(content=content, cursor=next_cursor)
        except Exception as e:
            logger.error("Failed to get command logs", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def create_session(self, *, working_directory: str | None = None) -> str:
        from opensandbox.api.execd.api.command.create_session import (
            sync as create_session_sync,
        )
        from opensandbox.api.execd.models.create_session_request import (
            CreateSessionRequest,
        )
        from opensandbox.api.execd.models.create_session_response import (
            CreateSessionResponse,
        )
        from opensandbox.api.execd.types import UNSET

        body = (
            CreateSessionRequest(cwd=working_directory)
            if working_directory
            else UNSET
        )
        try:
            parsed = create_session_sync(client=self._client, body=body)
            if parsed is None:
                raise SandboxApiException(
                    message="create_session returned no body",
                    status_code=0,
                )
            if isinstance(parsed, CreateSessionResponse):
                return parsed.session_id
            handle_api_error(parsed, "create_session")
            raise SandboxApiException(
                message="create_session unexpected response",
                status_code=200,
            )
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def run_in_session(
        self,
        session_id: str,
        command: str,
        *,
        working_directory: str | None = None,
        timeout: timedelta | None = None,
        handlers: ExecutionHandlersSync | None = None,
    ) -> Execution:
        if not (session_id and session_id.strip()):
            raise InvalidArgumentException("session_id cannot be empty")
        if not (command and command.strip()):
            raise InvalidArgumentException("command cannot be empty")

        timeout_ms = _resolve_run_in_session_timeout(timeout)
        body = _build_run_in_session_request_body(
            command, working_directory, timeout_ms
        )
        url = self._get_execd_url(
            self.RUN_IN_SESSION_PATH.format(session_id=session_id)
        )
        try:
            return self._execute_streaming_request(
                url=url,
                json_body=body.to_dict(),
                handlers=handlers,
                infer_exit_code=True,
                failure_message="run_in_session failed",
            )
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def delete_session(self, session_id: str) -> None:
        if not (session_id and session_id.strip()):
            raise InvalidArgumentException("session_id cannot be empty")
        from opensandbox.api.execd.api.command.delete_session import (
            sync as delete_session_sync,
        )

        try:
            parsed = delete_session_sync(
                client=self._client, session_id=session_id
            )
            if parsed is not None:
                handle_api_error(parsed, "delete_session")
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e
