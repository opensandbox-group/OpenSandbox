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
Dispatcher for processing execution events.
"""

from opensandbox.adapters.converter.event_node import EventNode
from opensandbox.models.execd import (
    Execution,
    ExecutionComplete,
    ExecutionError,
    ExecutionHandlers,
    ExecutionInit,
    ExecutionResult,
    OutputMessage,
)


class ExecutionEventDispatcher:
    """
    Dispatches events from the server stream to the Execution object and handlers.
    """

    def __init__(
        self,
        execution: Execution,
        handlers: ExecutionHandlers | None = None,
    ) -> None:
        self.execution = execution
        self.handlers = handlers

    async def dispatch(self, event_node: EventNode) -> None:
        """Dispatch a single event node asynchronously."""
        event_type = event_node.type
        timestamp = event_node.timestamp

        if event_type == "stdout":
            await self._handle_stdout(event_node, timestamp)
        elif event_type == "stderr":
            await self._handle_stderr(event_node, timestamp)
        elif event_type == "result":
            await self._handle_result(event_node, timestamp)
        elif event_type == "error":
            await self._handle_error(event_node, timestamp)
        elif event_type == "execution_complete":
            await self._handle_execution_complete(event_node, timestamp)
        elif event_type == "init":
            await self._handle_init(event_node, timestamp)
        elif event_type == "execution_count":
            if event_node.execution_count is not None:
                self.execution.execution_count = event_node.execution_count

    async def _handle_init(self, event_node: EventNode, timestamp: int) -> None:
        execution_id = event_node.text or ""
        init_event = ExecutionInit(
            id=execution_id,
            timestamp=timestamp,
        )
        self.execution.id = init_event.id
        if self.handlers and self.handlers.on_init:
            await self.handlers.on_init(init_event)

    async def _handle_stdout(self, event_node: EventNode, timestamp: int) -> None:
        text = event_node.text or ""
        message = OutputMessage(
            text=text,
            timestamp=timestamp,
            is_error=False,
        )
        self.execution.logs.add_stdout(message)
        if self.handlers and self.handlers.on_stdout:
            await self.handlers.on_stdout(message)

    async def _handle_stderr(self, event_node: EventNode, timestamp: int) -> None:
        text = event_node.text or ""
        message = OutputMessage(
            text=text,
            timestamp=timestamp,
            is_error=True,
        )
        self.execution.logs.add_stderr(message)
        if self.handlers and self.handlers.on_stderr:
            await self.handlers.on_stderr(message)

    async def _handle_result(self, event_node: EventNode, timestamp: int) -> None:
        result_text = event_node.results.get_text() if event_node.results else ""
        result = ExecutionResult(
            text=result_text,
            timestamp=timestamp,
        )
        self.execution.add_result(result)
        if self.handlers and self.handlers.on_result:
            await self.handlers.on_result(result)

    async def _handle_error(self, event_node: EventNode, timestamp: int) -> None:
        if not event_node.error:
            return

        error_data = event_node.error
        error = ExecutionError(
            name=error_data.name or "",
            value=error_data.value or "",
            timestamp=timestamp,
            traceback=error_data.traceback,
        )
        self.execution.error = error
        if self.handlers and self.handlers.on_error:
            await self.handlers.on_error(error)

    async def _handle_execution_complete(self, event_node: EventNode, timestamp: int) -> None:
        complete = ExecutionComplete(
            timestamp=timestamp,
            execution_time_in_millis=event_node.execution_time_in_millis or 0,
        )
        self.execution.complete = complete
        if self.handlers and self.handlers.on_execution_complete:
            await self.handlers.on_execution_complete(complete)
