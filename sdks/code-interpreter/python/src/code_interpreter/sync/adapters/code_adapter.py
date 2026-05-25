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
Synchronous adapter for code execution service (including SSE streaming).
"""

import json
import logging
import time

import httpx
from opensandbox.adapters.converter.event_node import EventNode
from opensandbox.adapters.converter.exception_converter import (
    ExceptionConverter,
)
from opensandbox.adapters.converter.response_handler import (
    extract_request_id,
    handle_api_error,
    require_parsed,
)
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import InvalidArgumentException, SandboxApiException
from opensandbox.models.execd import Execution
from opensandbox.models.execd_sync import ExecutionHandlersSync
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.converter.execution_event_dispatcher import (
    ExecutionEventDispatcherSync,
)

from code_interpreter.models.code_sync import CodeContextSync, SupportedLanguageSync
from code_interpreter.sync.services.code import CodesSync

logger = logging.getLogger(__name__)


def _normalize_sse_event(event_dict: dict) -> dict:
    if "type" in event_dict and "timestamp" in event_dict:
        return event_dict
    if "code" in event_dict and "message" in event_dict:
        return {
            "type": "error",
            "timestamp": int(time.time() * 1000),
            "error": {
                "ename": str(event_dict["code"]),
                "evalue": str(event_dict["message"]),
                "traceback": [],
            },
        }
    return event_dict


class CodesAdapterSync(CodesSync):
    """
    Synchronous adapter for code execution service.

    This adapter is the sync counterpart of :class:`code_interpreter.adapters.code_adapter.CodesAdapter`.
    It wraps the generated execd API client for non-streaming operations and uses direct ``httpx``
    streaming for SSE output while running code.

    Notes:

    - ``run`` performs blocking SSE streaming via ``httpx.Client.stream``.
    - Each SSE line is parsed into an :class:`EventNode` and dispatched via
      :class:`ExecutionEventDispatcherSync` to update the shared :class:`Execution` object
      and invoke any user-provided handlers.
    """

    RUN_CODE_PATH = "/code"
    CREATE_CONTEXT_PATH = "/code/context"

    def __init__(
        self, execd_endpoint: SandboxEndpoint, connection_config: ConnectionConfigSync
    ) -> None:
        """
        Initialize the code service adapter (sync).

        Args:
            execd_endpoint: Endpoint for execd daemon connection
            connection_config: Shared connection configuration (transport, headers, timeouts)
        """
        self.execd_endpoint = execd_endpoint
        self.connection_config = connection_config
        from opensandbox.api.execd import Client

        base_url = f"{self.connection_config.protocol}://{self.execd_endpoint.endpoint}"
        timeout_seconds = self.connection_config.request_timeout.total_seconds()
        timeout = httpx.Timeout(timeout_seconds)
        headers = {
            "User-Agent": self.connection_config.user_agent,
            **self.connection_config.headers,
            **(self.execd_endpoint.headers or {}),
        }

        self._client = Client(base_url=base_url, timeout=timeout)
        self._httpx_client = httpx.Client(
            base_url=base_url,
            headers=headers,
            timeout=timeout,
            transport=self.connection_config.transport,
        )
        self._client.set_httpx_client(self._httpx_client)

        sse_headers = {
            **headers,
            "Accept": "text/event-stream",
            "Cache-Control": "no-cache",
        }
        self._sse_client = httpx.Client(
            headers=sse_headers,
            timeout=httpx.Timeout(
                connect=timeout_seconds, read=None, write=timeout_seconds, pool=None
            ),
            transport=self.connection_config.transport,
        )

    def _get_execd_url(self, path: str) -> str:
        """Build URL for execd endpoint."""
        return (
            f"{self.connection_config.protocol}://{self.execd_endpoint.endpoint}{path}"
        )

    def create_context(self, language: str) -> CodeContextSync:
        """
        Create a new execution context for code interpretation (sync).

        Uses the generated API client for this non-streaming operation.
        """
        try:
            from opensandbox.api.execd.api.code_interpreting import create_code_context
            from opensandbox.api.execd.models.code_context import (
                CodeContext as ApiCodeContext,
            )
            from opensandbox.api.execd.models.code_context_request import (
                CodeContextRequest,
            )
            from opensandbox.api.execd.types import Unset

            response_obj = create_code_context.sync_detailed(
                client=self._client,
                body=CodeContextRequest(language=language),
            )
            handle_api_error(response_obj, "Create code context")
            parsed = require_parsed(response_obj, ApiCodeContext, "Create code context")
            context_id = None if isinstance(parsed.id, Unset) else parsed.id
            return CodeContextSync(id=context_id, language=parsed.language)
        except Exception as e:
            logger.error("Failed to create context", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def get_context(self, context_id: str) -> CodeContextSync:
        try:
            from opensandbox.api.execd.api.code_interpreting import get_context
            from opensandbox.api.execd.models.code_context import (
                CodeContext as ApiCodeContext,
            )
            from opensandbox.api.execd.types import Unset

            response_obj = get_context.sync_detailed(
                client=self._client,
                context_id=context_id,
            )
            handle_api_error(response_obj, "Get code context")
            parsed = require_parsed(response_obj, ApiCodeContext, "Get code context")
            context_id_val = None if isinstance(parsed.id, Unset) else parsed.id
            return CodeContextSync(id=context_id_val, language=parsed.language)
        except Exception as e:
            logger.error("Failed to get context", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def list_contexts(self, language: str) -> list[CodeContextSync]:
        try:
            from opensandbox.api.execd.api.code_interpreting import list_contexts
            from opensandbox.api.execd.types import UNSET

            response_obj = list_contexts.sync_detailed(
                client=self._client,
                language=language,
            )
            handle_api_error(response_obj, "List code contexts")
            parsed_list = require_parsed(response_obj, list, "List code contexts")
            result: list[CodeContextSync] = []
            for c in parsed_list:
                # c is an API CodeContext model
                context_id_val = c.id if c.id is not UNSET else None
                result.append(CodeContextSync(id=context_id_val, language=c.language))
            return result
        except Exception as e:
            logger.error("Failed to list contexts", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def delete_context(self, context_id: str) -> None:
        try:
            from opensandbox.api.execd.api.code_interpreting import delete_context

            response_obj = delete_context.sync_detailed(
                client=self._client,
                context_id=context_id,
            )
            handle_api_error(response_obj, "Delete code context")
        except Exception as e:
            logger.error("Failed to delete context", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def delete_contexts(self, language: str) -> None:
        try:
            from opensandbox.api.execd.api.code_interpreting import (
                delete_contexts_by_language,
            )

            response_obj = delete_contexts_by_language.sync_detailed(
                client=self._client,
                language=language,
            )
            handle_api_error(response_obj, "Delete code contexts by language")
        except Exception as e:
            logger.error("Failed to delete contexts", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def run(
        self,
        code: str,
        *,
        language: str | None = None,
        context: CodeContextSync | None = None,
        handlers: ExecutionHandlersSync | None = None,
    ) -> Execution:
        """
        Execute code within the specified context using SSE streaming (sync).

        Args:
            code: Source code to execute.
            context: Execution context (language + optional id). If None, a temporary Python context is used.
            handlers: Optional streaming handlers for stdout/stderr/events.

        Returns:
            Execution result populated incrementally while streaming events

        Raises:
            InvalidArgumentException: if code is empty
            SandboxApiException: if execd returns a non-200 response
            SandboxException: for other errors converted by :class:`ExceptionConverter`
        """
        if not code.strip():
            raise InvalidArgumentException("Code cannot be empty")

        try:
            if (
                context is not None
                and language is not None
                and context.language != language
            ):
                raise InvalidArgumentException(
                    f"language '{language}' must match context.language '{context.language}'"
                )

            if context is None:
                # Default context: language default context (server-side behavior).
                # When context.id is omitted, execd will create/reuse a default session per language.
                context = CodeContextSync(
                    language=language or SupportedLanguageSync.PYTHON
                )
            api_request = {
                "code": code,
                "context": {
                    "language": context.language,
                    **({"id": context.id} if context.id else {}),
                },
            }

            url = self._get_execd_url(self.RUN_CODE_PATH)
            execution = Execution(id=None, execution_count=None, result=[], error=None)
            dispatcher = ExecutionEventDispatcherSync(execution, handlers)

            with self._sse_client.stream("POST", url, json=api_request) as response:
                if response.status_code != 200:
                    response.read()
                    raise SandboxApiException(
                        message=f"Failed to run code. Status code: {response.status_code}",
                        status_code=response.status_code,
                        request_id=extract_request_id(response.headers),
                    )

                for line in response.iter_lines():
                    if not line or not line.strip():
                        continue
                    data = line
                    if data.startswith("data:"):
                        data = data[5:].strip()
                    try:
                        event_dict = _normalize_sse_event(json.loads(data))
                        event_node = EventNode(**event_dict)
                        dispatcher.dispatch(event_node)
                    except json.JSONDecodeError:
                        logger.debug("Failed to parse SSE line: %s", line)
                        continue
                    except Exception as e:
                        logger.error("Error processing event: %s", data, exc_info=e)
                        continue

            return execution
        except Exception as e:
            logger.error("Failed to run code (length: %s)", len(code), exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def interrupt(self, execution_id: str) -> None:
        """
        Interrupt a currently running code execution.

        Args:
            execution_id: Execution id returned by execd for the running code execution
        """
        try:
            from opensandbox.api.execd.api.code_interpreting import interrupt_code

            response_obj = interrupt_code.sync_detailed(
                client=self._client, id=execution_id
            )
            handle_api_error(response_obj, "Interrupt code execution")
        except Exception as e:
            logger.error("Failed to interrupt code execution", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e
