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
Adapter implementation for code execution service.

Provides the concrete implementation of Codes by wrapping auto-generated
API clients and handling SSE streaming for real-time code execution.
"""

import json
import logging
import time

import httpx
from opensandbox.adapters.converter.event_node import EventNode
from opensandbox.adapters.converter.exception_converter import (
    ExceptionConverter,
)
from opensandbox.adapters.converter.execution_event_dispatcher import (
    ExecutionEventDispatcher,
)
from opensandbox.adapters.converter.response_handler import (
    extract_request_id,
    handle_api_error,
    require_parsed,
)
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import InvalidArgumentException, SandboxApiException
from opensandbox.models.execd import Execution, ExecutionHandlers
from opensandbox.models.sandboxes import SandboxEndpoint

from code_interpreter.adapters.converter.code_execution_converter import (
    CodeExecutionConverter,
)
from code_interpreter.models.code import CodeContext, SupportedLanguage
from code_interpreter.services.code import Codes

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


class CodesAdapter(Codes):
    """
    Adapter implementation for code execution service.

    This adapter wraps auto-generated API clients and provides the concrete
    implementation of the Codes interface. It handles both standard
    API calls and SSE streaming for real-time code execution output.

    Similar to CommandServiceAdapter, this adapter uses:
    - Generated API clients for simple operations (create_context, interrupt)
    - Direct httpx SSE streaming for run
    - ExceptionConverter for unified exception handling
    """

    RUN_CODE_PATH = "/code"
    CREATE_CONTEXT_PATH = "/code/context"

    def __init__(
        self, execd_endpoint: SandboxEndpoint, connection_config: ConnectionConfig
    ) -> None:
        """
        Initialize the code service adapter.

        Args:
            execd_endpoint: Endpoint for execd daemon connection
            connection_config: Shared connection configuration (transport, headers, timeouts)
        """
        self.execd_endpoint = execd_endpoint
        self.connection_config = connection_config
        from opensandbox.api.execd import Client

        protocol = self.connection_config.protocol
        base_url = f"{protocol}://{self.execd_endpoint.endpoint}"
        timeout_seconds = self.connection_config.request_timeout.total_seconds()
        timeout = httpx.Timeout(timeout_seconds)

        headers = {
            "User-Agent": self.connection_config.user_agent,
            **self.connection_config.headers,
            **self.execd_endpoint.headers,
        }

        # Execd API does not require authentication
        self._client = Client(
            base_url=base_url,
            timeout=timeout,
        )

        # Inject httpx client (adapter-owned)
        self._httpx_client = httpx.AsyncClient(
            base_url=base_url,
            headers=headers,
            timeout=timeout,
            transport=self.connection_config.transport,
        )
        self._client.set_async_httpx_client(self._httpx_client)

        # SSE client (read timeout disabled)
        sse_headers = {
            **headers,
            "Accept": "text/event-stream",
            "Cache-Control": "no-cache",
        }
        self._sse_client = httpx.AsyncClient(
            headers=sse_headers,
            timeout=httpx.Timeout(
                connect=timeout_seconds,
                read=None,
                write=timeout_seconds,
                pool=None,
            ),
            transport=self.connection_config.transport,
        )

    async def _get_client(self):
        """Return the client for execd API (no auth required)."""
        return self._client

    def _get_execd_url(self, path: str) -> str:
        """Build URL for execd endpoint."""
        protocol = self.connection_config.protocol
        return f"{protocol}://{self.execd_endpoint.endpoint}{path}"

    async def _get_sse_client(self) -> httpx.AsyncClient:
        """Return SSE client (read timeout disabled) for execd streaming."""
        return self._sse_client

    async def create_context(self, language: str) -> CodeContext:
        """
        Creates a new execution context for code interpretation.

        Uses the generated API client for this non-streaming operation.
        """
        try:
            from opensandbox.api.execd.api.code_interpreting import create_code_context
            from opensandbox.api.execd.models.code_context_request import (
                CodeContextRequest,
            )

            client = await self._get_client()
            api_request = CodeContextRequest(language=language)

            response_obj = await create_code_context.asyncio_detailed(
                client=client,
                body=api_request,
            )

            handle_api_error(response_obj, "Create code context")
            from opensandbox.api.execd.models.code_context import (
                CodeContext as ApiCodeContext,
            )

            parsed = require_parsed(response_obj, ApiCodeContext, "Create code context")
            return CodeExecutionConverter.from_api_code_context(parsed)

        except Exception as e:
            logger.error("Failed to create context", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def get_context(self, context_id: str) -> CodeContext:
        try:
            from opensandbox.api.execd.api.code_interpreting import get_context
            from opensandbox.api.execd.models.code_context import (
                CodeContext as ApiCodeContext,
            )

            client = await self._get_client()
            response_obj = await get_context.asyncio_detailed(
                client=client,
                context_id=context_id,
            )
            handle_api_error(response_obj, "Get code context")
            parsed = require_parsed(response_obj, ApiCodeContext, "Get code context")
            return CodeExecutionConverter.from_api_code_context(parsed)
        except Exception as e:
            logger.error("Failed to get context", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def list_contexts(self, language: str) -> list[CodeContext]:
        try:
            from opensandbox.api.execd.api.code_interpreting import list_contexts

            client = await self._get_client()
            response_obj = await list_contexts.asyncio_detailed(
                client=client,
                language=language,
            )
            handle_api_error(response_obj, "List code contexts")
            parsed_list = require_parsed(response_obj, list, "List code contexts")
            return [CodeExecutionConverter.from_api_code_context(c) for c in parsed_list]
        except Exception as e:
            logger.error("Failed to list contexts", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def delete_context(self, context_id: str) -> None:
        try:
            from opensandbox.api.execd.api.code_interpreting import delete_context

            client = await self._get_client()
            response_obj = await delete_context.asyncio_detailed(
                client=client,
                context_id=context_id,
            )
            handle_api_error(response_obj, "Delete code context")
        except Exception as e:
            logger.error("Failed to delete context", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def delete_contexts(self, language: str) -> None:
        try:
            from opensandbox.api.execd.api.code_interpreting import (
                delete_contexts_by_language,
            )

            client = await self._get_client()
            response_obj = await delete_contexts_by_language.asyncio_detailed(
                client=client,
                language=language,
            )
            handle_api_error(response_obj, "Delete code contexts by language")
        except Exception as e:
            logger.error("Failed to delete contexts", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def run(
        self,
        code: str,
        *,
        language: str | None = None,
        context: CodeContext | None = None,
        handlers: ExecutionHandlers | None = None,
    ) -> Execution:
        """
        Executes code within the specified context using SSE streaming.

        Similar to CommandServiceAdapter.run, this uses direct httpx
        streaming to handle SSE responses from the execd service.
        """
        if not code.strip():
            raise InvalidArgumentException("Code cannot be empty")

        try:
            if context is not None and language is not None and context.language != language:
                raise InvalidArgumentException(
                    f"language '{language}' must match context.language '{context.language}'"
                )

            # Default context: language default context (server-side behavior).
            # When context.id is omitted, execd will create/reuse a default session per language.
            if context is None:
                context = CodeContext(language=language or SupportedLanguage.PYTHON)
            api_request = CodeExecutionConverter.to_api_run_code_request(code, context)

            # Prepare URL
            url = self._get_execd_url(self.RUN_CODE_PATH)

            execution = Execution(
                id=None,
                execution_count=None,
                result=[],
                error=None,
            )

            # Use SSE client for streaming responses (read timeout disabled)
            client = await self._get_sse_client()

            # Use streaming request for SSE
            async with client.stream("POST", url, json=api_request) as response:
                if response.status_code != 200:
                    await response.aread()
                    error_body = response.text
                    logger.error(
                        "Failed to run code. Status: %s, Body: %s",
                        response.status_code,
                        error_body,
                    )
                    raise SandboxApiException(
                        message=f"Failed to run code. Status code: {response.status_code}",
                        status_code=response.status_code,
                        request_id=extract_request_id(response.headers),
                    )

                dispatcher = ExecutionEventDispatcher(execution, handlers)

                async for line in response.aiter_lines():
                    if not line.strip():
                        continue

                    # Handle potential SSE format "data: ..."
                    data = line
                    if data.startswith("data:"):
                        data = data[5:].strip()

                    try:
                        event_dict = _normalize_sse_event(json.loads(data))
                        event_node = EventNode(**event_dict)
                        await dispatcher.dispatch(event_node)
                    except json.JSONDecodeError:
                        logger.debug("Failed to parse SSE line: %s", line)
                        continue
                    except Exception as e:
                        logger.error("Error processing event: %s", data, exc_info=e)
                        continue

            return execution

        except Exception as e:
            logger.error(
                "Failed to run code (length: %s)", len(code), exc_info=e
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def interrupt(self, execution_id: str) -> None:
        """
        Interrupts a currently running code execution.

        Uses the generated API client for this operation.
        """
        try:
            from opensandbox.api.execd.api.code_interpreting import interrupt_code

            client = await self._get_client()
            response_obj = await interrupt_code.asyncio_detailed(
                client=client,
                id=execution_id,
            )

            handle_api_error(response_obj, "Interrupt code execution")

        except Exception as e:
            logger.error("Failed to interrupt code execution", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e
