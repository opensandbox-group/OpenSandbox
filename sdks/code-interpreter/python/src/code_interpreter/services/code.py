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
Code execution service interface.

Defines the contract for multi-language code interpretation with context management,
session persistence, and real-time execution capabilities.
"""

from typing import Protocol, overload

from opensandbox.models.execd import Execution, ExecutionHandlers

from code_interpreter.models.code import CodeContext


class Codes(Protocol):
    """
    Code execution service for multi-language code interpretation.

    This service provides advanced code execution capabilities with context management,
    session persistence, and multi-language support.

    Supported Languages:

    - Python: Full Python 3.x support with package management
    - JavaScript/Node.js: ES6+ with npm package support
    - TypeScript: With Node.js runtime support
    - Bash: Shell scripting with full system access
    - Java: Compilation and execution with classpath management
    - Go: Script and compiled Go execution

    Key Features:

    - Execution Contexts: Isolated environments with persistent state
    - Variable Persistence: Variables and imports persist across executions
    - Real-time Interruption: Stop long-running code execution safely
    - Output Streaming: Real-time stdout/stderr with proper buffering
    - Error Handling: Language-specific error parsing and reporting

    Usage Example:

    ```python
    # Create execution context
    context = await code_service.create_context(SupportedLanguage.PYTHON)

    # Execute code with persistent state
    result1 = await code_service.run(
        "import numpy as np; x = 42",
        context=context,
    )

    result2 = await code_service.run(
        "print(f'Value: {x}, NumPy version: {np.__version__}')",
        context=context,
    )
    # Variables 'x' and 'np' persist between executions
    ```
    """

    async def create_context(self, language: str) -> CodeContext:
        """
        Creates a new execution context for code interpretation.

        An execution context maintains the state of variables, imports, and working
        directory across multiple code executions. This allows for interactive
        programming sessions where subsequent code can reference previously
        defined variables and functions.

        Args:
            language: The programming language for this context (e.g., "python", "javascript")

        Returns:
            A new CodeContext with the specified configuration

        Raises:
            SandboxException: If the language is not supported or context creation fails
        """
        ...

    async def get_context(self, context_id: str) -> CodeContext:
        """
        Get an existing execution context by id.

        Args:
            context_id: Context/session id

        Returns:
            The existing CodeContext
        """
        ...

    async def list_contexts(self, language: str) -> list[CodeContext]:
        """
        List active contexts under a given language/runtime.

        Args:
            language: Execution runtime (e.g. "python", "bash")

        Returns:
            List of contexts
        """
        ...

    async def delete_context(self, context_id: str) -> None:
        """
        Delete an execution context by id.

        Args:
            context_id: Context/session id to delete
        """
        ...

    async def delete_contexts(self, language: str) -> None:
        """
        Delete all execution contexts under a given language/runtime.

        Args:
            language: Execution runtime (e.g. "python", "bash")
        """
        ...

    @overload
    async def run(
        self,
        code: str,
        *,
        context: CodeContext,
        handlers: ExecutionHandlers | None = None,
    ) -> Execution: ...

    @overload
    async def run(
        self,
        code: str,
        *,
        language: str,
        handlers: ExecutionHandlers | None = None,
    ) -> Execution: ...

    async def run(
        self,
        code: str,
        *,
        language: str | None = None,
        context: CodeContext | None = None,
        handlers: ExecutionHandlers | None = None,
    ) -> Execution:
        """
        Executes code within the specified context.

        This method runs the provided code string in the language interpreter,
        capturing all output, errors, and execution metadata. The execution
        happens within the context's environment, preserving variable state
        and working directory.

        Execution Behavior:

        - Asynchronous: Non-blocking execution with proper async handling
        - Stateful: Variables and imports persist in the context
        - Streaming: Output is captured in real-time as it's produced
        - Interruptible: Can be stopped using interrupt() method

        Args:
            code: Source code to execute.
            language: Convenience language selector for this run. If provided and ``context`` is None,
                a **default context for this language** is used (execd will create/reuse a default
                session when ``context.id`` is omitted). If both ``language`` and ``context`` are
                provided, they must match.
            context: Execution context (language + optional id). If None, the default Python context is used.
            handlers: Optional streaming handlers for stdout/stderr/events.

        Returns:
            Execution with stdout, stderr, exit code, and execution metadata

        Raises:
            SandboxException: If execution fails or times out
        """
        ...

    async def interrupt(self, execution_id: str) -> None:
        """
        Interrupts a currently running code execution.

        This method safely terminates a running code execution, cleaning up
        resources and ensuring the interpreter remains in a consistent state.
        The interruption is cooperative and may take some time to complete.

        Interruption Behavior:

        - Safe: Preserves interpreter state and doesn't corrupt the context
        - Cooperative: Respects language-specific interruption mechanisms
        - Timeout: Will force-kill after a reasonable timeout if needed

        Args:
            execution_id: The unique identifier of the execution to interrupt

        Raises:
            SandboxException: If interruption fails
        """
        ...
