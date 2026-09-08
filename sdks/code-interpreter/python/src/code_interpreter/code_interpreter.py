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
Code Interpreter SDK providing secure, isolated code execution capabilities.

This module provides the main CodeInterpreter class that extends basic Sandbox
functionality with specialized code execution features, including multi-language
support, session management, and variable persistence.
"""

import asyncio
import logging
import time
from datetime import timedelta

from opensandbox.exceptions import (
    InvalidArgumentException,
    SandboxException,
    SandboxInternalException,
    SandboxReadyTimeoutException,
)
from opensandbox.sandbox import Sandbox

from code_interpreter.adapters.factory import AdapterFactory
from code_interpreter.services.code import Codes

logger = logging.getLogger(__name__)

DEFAULT_READY_TIMEOUT = timedelta(seconds=30)
DEFAULT_HEALTH_CHECK_POLLING_INTERVAL = timedelta(milliseconds=200)

# Strict health check script: verifies the code interpreter runtime (Jupyter
# kernel gateway) is actually serving inside the sandbox. execd starts serving
# /ping before the entrypoint launches Jupyter, and the setup stage may run
# short-lived "jupyter kernelspec" helpers, so a daemon ping or a process-name
# grep cannot prove the runtime is ready. Probing the Jupyter listen port
# (127.0.0.1:${JUPYTER_PORT:-44771}, same default as the entrypoint) only
# passes once the server accepts connections.
RUNTIME_PROCESS_CHECK_COMMAND = (
    "bash -c 'exec 3<>/dev/tcp/127.0.0.1/${JUPYTER_PORT:-44771}' "
    "&& exit 0 || exit 1"
)


class CodeInterpreter:
    """
    Code Interpreter SDK providing secure, isolated code execution capabilities.

    This class extends the basic Sandbox functionality with specialized code execution features,
    including multi-language support, session management, and variable persistence.

    Key Features:

    - Multi-language Code Execution: Support for Python, JavaScript, Bash, Java, Kotlin
    - Session Management: Persistent execution contexts with variable state
    - Sandbox Integration: Full access to underlying sandbox file system and command execution
    - Streaming Execution: Real-time code execution with output streaming
    - Variable Inspection: Access to execution variables and state

    Usage Example:

    ```python
    # First create a sandbox instance

    sandbox = await Sandbox.create(
        "python:3.11",
        resource={"cpu": "1", "memory": "2Gi"}
    )

    # Then create a code interpreter wrapping the sandbox
    interpreter = await CodeInterpreter.create(sandbox=sandbox)

    # Execute code with context
    from code_interpreter.models.code import SupportedLanguage
    context = await interpreter.codes.create_context(SupportedLanguage.PYTHON)
    result = await interpreter.codes.run("print('Hello World')", context=context)
    print(result.logs.stdout)  # Output: Hello World

    # Access underlying sandbox for file operations
    await interpreter.sandbox.files.write_files([
        WriteEntry(path="data.txt", data="Hello")
    ])
    file_result = await interpreter.codes.run(
        "with open('data.txt') as f: print(f.read())",
        context=context,
    )

    # Always clean up resources
    await sandbox.kill()
    await sandbox.close()
    ```
    """

    def __init__(self, sandbox: Sandbox, code_service: Codes) -> None:
        """
        Initialize CodeInterpreter with sandbox and code service.

        Note: This constructor is for internal use. Use CodeInterpreter.create() instead.

        Args:
            sandbox: Underlying sandbox instance
            code_service: Code execution implementation
        """
        self._sandbox = sandbox
        self._code_service = code_service

    @property
    def sandbox(self) -> Sandbox:
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

        Allows writing, reading, listing, and deleting files and directories.

        Returns:
            Service for filesystem manipulation
        """
        return self._sandbox.files

    @property
    def commands(self):
        """
        Provides access to command execution operations.

        Allows running shell commands, capturing output, and managing processes.

        Returns:
            Service for command execution
        """
        return self._sandbox.commands

    @property
    def metrics(self):
        """
        Provides access to sandbox metrics and monitoring.

        Allows retrieving resource usage statistics (CPU, memory) and other performance metrics.

        Returns:
            Service for metrics retrieval
        """
        return self._sandbox.metrics

    @property
    def codes(self) -> Codes:
        """
        Provides access to code execution operations.

        This service enables:
        - Multi-language code execution (Python, JavaScript, Bash, etc.)
        - Execution context management with persistent variables
        - Real-time output streaming and interruption capabilities

        Returns:
            Service for advanced code execution with session support
        """
        return self._code_service

    async def ping(self) -> bool:
        """
        Check if the code execution service (execd) is responsive.

        Returns:
            True if the code execution service is responsive, False otherwise
        """
        return await self._code_service.ping()

    async def is_healthy(self) -> bool:
        """
        Check if the code interpreter is healthy (strict check).

        Healthy means both:

        - the code execution service (execd) answers ``GET /ping``; and
        - the code interpreter runtime (Jupyter kernel gateway) is serving
          inside the sandbox, verified by probing its listen port through the
          execd command API.

        Exceptions raised by either leg are treated as unhealthy.

        Returns:
            True if healthy, False otherwise
        """
        try:
            return await self.ping() and await self._is_runtime_process_alive()
        except Exception:
            return False

    async def _is_runtime_process_alive(self) -> bool:
        """
        Check if the code interpreter runtime (Jupyter) is serving.

        Probes the runtime listen port through the sandbox command service and
        treats a failed command (non-zero exit surfaced as an execution error)
        as an unhealthy runtime.
        """
        try:
            execution = await self._sandbox.commands.run(
                RUNTIME_PROCESS_CHECK_COMMAND
            )
            return execution.error is None
        except Exception:
            return False

    async def check_ready(
        self,
        timeout: timedelta,
        polling_interval: timedelta,
    ) -> None:
        """
        Wait for the code interpreter to pass the strict health check with polling.

        Raises:
            SandboxReadyTimeoutException: if the health check doesn't pass within timeout
        """
        logger.info(
            f"Waiting for code interpreter {self.id} to pass health check "
            f"(timeout: {timeout.total_seconds()}s)"
        )

        deadline = time.monotonic() + timeout.total_seconds()
        attempt = 0

        while time.monotonic() < deadline:
            attempt += 1
            if await self.is_healthy():
                logger.info(
                    f"Code interpreter {self.id} passed health check "
                    f"after {attempt} attempts"
                )
                return

            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            await asyncio.sleep(min(polling_interval.total_seconds(), remaining))

        raise SandboxReadyTimeoutException(
            f"Code interpreter {self.id} health check timed out after "
            f"{timeout.total_seconds()}s ({attempt} attempts). The code execution "
            f"service (execd) or the interpreter runtime (Jupyter) did not become "
            f"ready. Pass skip_health_check=True to skip this check."
        )

    @classmethod
    async def create(
        cls,
        sandbox: Sandbox,
        *,
        ready_timeout: timedelta = DEFAULT_READY_TIMEOUT,
        health_check_polling_interval: timedelta = DEFAULT_HEALTH_CHECK_POLLING_INTERVAL,
        skip_health_check: bool = False,
    ) -> "CodeInterpreter":
        """
        Creates a CodeInterpreter from an existing Sandbox instance.

        This factory method handles the creation and initialization of CodeInterpreter
        services, including the code execution service and language configuration.

        By default a strict health check runs before the interpreter is returned:
        the code execution service (execd) must answer ``GET /ping`` AND the
        code interpreter runtime (Jupyter kernel gateway) must be serving,
        both within ``ready_timeout``. execd starts serving before the runtime
        launches, so the daemon ping alone is not enough. Set
        ``skip_health_check=True`` to opt out.

        CodeInterpreter must be created by wrapping an existing Sandbox instance with
        code execution capabilities. This design ensures clear separation of concerns:
        - Sandbox handles infrastructure (containers, resources, networking)
        - CodeInterpreter adds code execution capabilities on top

        Args:
            sandbox: Existing sandbox instance to wrap with code execution capabilities
            ready_timeout: Maximum time to wait for the code execution service health check
            health_check_polling_interval: Time between health check attempts
            skip_health_check: If True, do not wait for the code execution service
                to become ready; the returned interpreter may fail on first use

        Returns:
            CodeInterpreter instance wrapping the sandbox

        Raises:
            InvalidArgumentException: If sandbox is not provided
            SandboxException: If creation fails
            SandboxReadyTimeoutException: If the code execution service health check
                times out and ``skip_health_check`` is False
            SandboxInternalException: If internal service initialization fails
        """
        if sandbox is None:
            raise InvalidArgumentException("Sandbox instance must be provided")

        logger.info(f"Creating code interpreter from sandbox: {sandbox.id}")

        factory = AdapterFactory(sandbox.connection_config)

        try:
            # Connect to the execd daemon endpoint for code execution services
            from opensandbox.constants import DEFAULT_EXECD_PORT

            code_interpreter_endpoint = await sandbox.get_endpoint(DEFAULT_EXECD_PORT)
            code_execution_service = factory.create_code_execution_service(
                code_interpreter_endpoint
            )

            interpreter = cls(sandbox, code_execution_service)

            if not skip_health_check:
                await interpreter.check_ready(ready_timeout, health_check_polling_interval)

            logger.info(f"Code interpreter {sandbox.id} created successfully")

            return interpreter
        except Exception as e:
            if isinstance(e, SandboxException):
                raise
            raise SandboxInternalException(
                f"Failed to create code interpreter: {e}", cause=e
            ) from e
