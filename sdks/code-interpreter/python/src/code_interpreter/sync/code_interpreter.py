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
Synchronous Code Interpreter SDK.
"""

import logging

from opensandbox.constants import DEFAULT_EXECD_PORT
from opensandbox.exceptions import (
    InvalidArgumentException,
    SandboxException,
    SandboxInternalException,
)
from opensandbox.sync.sandbox import SandboxSync

from code_interpreter.sync.adapters.factory import AdapterFactorySync
from code_interpreter.sync.services.code import CodesSync

logger = logging.getLogger(__name__)


class CodeInterpreterSync:
    """
    Synchronous Code Interpreter SDK providing secure, isolated code execution capabilities.

    This class mirrors the async :class:`code_interpreter.code_interpreter.CodeInterpreter`, but all
    operations are **blocking** and executed in the current thread.

    It wraps an existing :class:`opensandbox.sync.sandbox.SandboxSync` instance and adds
    code-execution APIs (contexts, run with SSE streaming, interrupts) on top.

    Notes:

    - **Blocking**: Do not call these methods directly from an asyncio event loop thread.
      If you need non-blocking behavior, prefer the async :class:`~code_interpreter.code_interpreter.CodeInterpreter`.
    - **Lifecycle**: Remote lifecycle is owned by the underlying sandbox; call methods on
      ``interpreter.sandbox`` for pause/resume/kill/renew/metrics/info/endpoints.

    Usage Example:

    ```python
    from opensandbox.sync.sandbox import SandboxSync
    from code_interpreter.sync.code_interpreter import CodeInterpreterSync
    from code_interpreter.models.code import SupportedLanguage

    sandbox = SandboxSync.create("python:3.11")
    interpreter = CodeInterpreterSync.create(sandbox=sandbox)

    ctx = interpreter.codes.create_context(SupportedLanguage.PYTHON)
    result = interpreter.codes.run("print('hi')", context=ctx)

    sandbox.kill()
    sandbox.close()
    ```
    """

    def __init__(self, sandbox: SandboxSync, code_service: CodesSync) -> None:
        """
        Initialize CodeInterpreterSync with sandbox and code service.

        Note: This constructor is for internal use. Use :meth:`create` instead.

        Args:
            sandbox: Underlying sandbox instance
            code_service: Code execution service implementation (sync)
        """
        self._sandbox = sandbox
        self._code_service = code_service

    @property
    def sandbox(self) -> SandboxSync:
        """
        Provides access to the underlying sandbox instance.

        Returns:
            The underlying sandbox instance
        """
        return self._sandbox

    @property
    def id(self) -> str:
        """
        Gets the unique identifier of this code interpreter (same as underlying sandbox ID).

        Returns:
            ID of the code interpreter/sandbox
        """
        return self._sandbox.id

    @property
    def files(self):
        """
        Provides access to file system operations within the sandbox.

        Returns:
            Service for filesystem manipulation
        """
        return self._sandbox.files

    @property
    def commands(self):
        """
        Provides access to command execution operations.

        Returns:
            Service for command execution
        """
        return self._sandbox.commands

    @property
    def metrics(self):
        """
        Provides access to sandbox metrics and monitoring.

        Returns:
            Service for metrics retrieval
        """
        return self._sandbox.metrics

    @property
    def codes(self) -> CodesSync:
        """
        Provides access to code execution operations (sync).

        This service enables:
        - Multi-language code execution (Python, JavaScript, Bash, etc.)
        - Execution context management with persistent variables
        - Real-time output streaming and interruption capabilities

        Returns:
            Service for advanced code execution with session support
        """
        return self._code_service

    @classmethod
    def create(cls, sandbox: SandboxSync) -> "CodeInterpreterSync":
        """
        Create a CodeInterpreterSync from an existing SandboxSync instance (blocking).

        Args:
            sandbox: Existing sandbox instance to wrap with code execution capabilities

        Returns:
            CodeInterpreterSync instance wrapping the sandbox

        Raises:
            InvalidArgumentException: If sandbox is not provided
            SandboxException: If creation fails
            SandboxInternalException: If internal service initialization fails
        """
        if sandbox is None:
            raise InvalidArgumentException("Sandbox instance must be provided")

        logger.info(f"Creating code interpreter from sandbox: {sandbox.id}")
        factory = AdapterFactorySync(sandbox.connection_config)
        try:
            endpoint = sandbox.get_endpoint(DEFAULT_EXECD_PORT)
            code_service = factory.create_code_execution_service(endpoint)
            logger.info(f"Code interpreter {sandbox.id} created successfully")
            return cls(sandbox, code_service)
        except Exception as e:
            if isinstance(e, SandboxException):
                raise
            raise SandboxInternalException(
                f"Failed to create code interpreter: {e}", cause=e
            ) from e
