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
Synchronous code execution service interface.

Defines the contract for multi-language code interpretation with context management,
session persistence, and real-time execution capabilities (SSE streaming), **in blocking form**.

This is the sync counterpart of :mod:`code_interpreter.services.code`.
"""

from typing import Protocol, overload

from opensandbox.models.execd import Execution
from opensandbox.models.execd_sync import ExecutionHandlersSync

from code_interpreter.models.code_sync import CodeContextSync


class CodesSync(Protocol):
    """
    Code execution service for multi-language code interpretation (sync).

    This service provides advanced code execution capabilities with context management,
    session persistence, and multi-language support.

    Supported Languages (typical):
        - Python
        - JavaScript / TypeScript
        - Bash
        - Java
        - Go

    Key Features:
        - Execution Contexts: Isolated environments with persistent state
        - Variable Persistence: Variables and imports persist across executions in a context
        - Real-time Interruption: Stop long-running code execution safely
        - Output Streaming: Real-time stdout/stderr via SSE

    Notes:
        - All methods are **blocking** and executed in the current thread.
        - For non-blocking usage, prefer the async :class:`code_interpreter.services.code.Codes`.
    """

    def create_context(self, language: str) -> CodeContextSync:
        """
        Create a new execution context for code interpretation (blocking).

        An execution context maintains state (variables/imports/working directory) across
        multiple code executions, enabling interactive sessions.

        Args:
            language: The programming language for this context (e.g., "python", "typescript").

        Returns:
            A new CodeContextSync.

        Raises:
            SandboxException: If the language is not supported or context creation fails.
        """
        ...

    def get_context(self, context_id: str) -> CodeContextSync:
        """Get an existing execution context by id (blocking)."""
        ...

    def list_contexts(self, language: str) -> list[CodeContextSync]:
        """List active contexts under a given language/runtime (blocking)."""
        ...

    def delete_context(self, context_id: str) -> None:
        """Delete an execution context by id (blocking)."""
        ...

    def delete_contexts(self, language: str) -> None:
        """Delete all contexts under a language/runtime (blocking)."""
        ...

    @overload
    def run(
        self,
        code: str,
        *,
        context: CodeContextSync,
        handlers: ExecutionHandlersSync | None = None,
    ) -> Execution: ...

    @overload
    def run(
        self,
        code: str,
        *,
        language: str,
        handlers: ExecutionHandlersSync | None = None,
    ) -> Execution: ...

    def run(
        self,
        code: str,
        *,
        language: str | None = None,
        context: CodeContextSync | None = None,
        handlers: ExecutionHandlersSync | None = None,
    ) -> Execution:
        """
        Execute code within the specified context (blocking).

        This method runs the provided code string in the language interpreter, capturing output,
        errors, and execution metadata. Execution happens within the context's environment,
        preserving variable state and working directory.

        Execution behavior:
            - Blocking: The call does not return until the stream finishes.
            - Stateful: Variables and imports persist in the context.
            - Streaming: Output is processed incrementally as SSE events arrive.
            - Interruptible: Can be stopped using :meth:`interrupt`.

        Args:
            code: Source code to execute.
            language: Convenience language selector for this run. If provided and ``context`` is None,
                a **default context for this language** is used (execd will create/reuse a default
                session when ``context.id`` is omitted). If both ``language`` and ``context`` are
                provided, they must match.
            context: Execution context (language + optional id). If None, the default Python context is used.
            handlers: Optional streaming handlers for stdout/stderr/events.

        Returns:
            Execution with stdout/stderr/events and execution metadata.

        Raises:
            SandboxException: If execution fails or times out.
        """
        ...

    def interrupt(self, execution_id: str) -> None:
        """
        Interrupt a currently running code execution.

        This method attempts to safely terminate a running execution, cleaning up resources and
        keeping the interpreter in a consistent state.

        Args:
            execution_id: The unique identifier of the execution to interrupt.

        Raises:
            SandboxException: If interruption fails.
        """
        ...
