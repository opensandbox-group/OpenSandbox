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
Execution-related data models.

Models for code execution, results, and output handling.
"""

from collections.abc import Awaitable, Callable
from datetime import datetime, timedelta
from typing import Any

from pydantic import BaseModel, ConfigDict, Field, model_validator


class OutputMessage(BaseModel):
    """
    Output message from code execution.

    Represents a single output message from either stdout or stderr streams
    during code execution, including timing information.
    """

    text: str = Field(description="The text content of the output message")
    timestamp: int = Field(
        description="Unix timestamp in milliseconds when message was generated"
    )
    is_error: bool = Field(
        default=False, description="True if message came from stderr"
    )

    model_config = ConfigDict(populate_by_name=True)


class ExecutionResult(BaseModel):
    """
    Result of code execution.

    Represents a single output result from code execution, which may include
    text content, formatting information, and timing data.
    """

    text: str | None = Field(default=None, description="UTF-8 encoded text content")
    timestamp: int = Field(
        description="Unix timestamp in milliseconds when result was created"
    )
    extra_properties: dict[str, str] = Field(
        default_factory=dict,
        description="Additional result content in UTF-8 format",
        alias="extra_properties",
    )

    model_config = ConfigDict(populate_by_name=True)


class ExecutionError(BaseModel):
    """
    Error information when code execution fails.

    Contains detailed error information following standard error reporting format,
    including error type, message, timing, and stack trace for debugging purposes.
    """

    name: str = Field(
        description="Error name/type (e.g., 'SyntaxError', 'RuntimeError')"
    )
    value: str = Field(description="Error message explaining what went wrong")
    timestamp: int = Field(
        description="Unix timestamp in milliseconds when error occurred"
    )
    traceback: list[str] = Field(default_factory=list, description="Stack trace lines")

    model_config = ConfigDict(populate_by_name=True)


class ExecutionLogs(BaseModel):
    """
    Container for execution output logs.

    Separates standard output and error output streams for better organization
    and allows users to process different types of output appropriately.
    """

    stdout: list["OutputMessage"] = Field(
        default_factory=list, description="Standard output messages"
    )
    stderr: list["OutputMessage"] = Field(
        default_factory=list, description="Standard error messages"
    )

    def add_stdout(self, message: OutputMessage) -> None:
        """Add a message to standard output log."""
        self.stdout.append(message)

    def add_stderr(self, message: OutputMessage) -> None:
        """Add a message to standard error log."""
        self.stderr.append(message)


class ExecutionComplete(BaseModel):
    """
    Execution completion event.

    Represents the completion of a code execution,
    including timing information about when the execution finished.
    """

    timestamp: int = Field(description="Unix timestamp when execution completed")
    execution_time_in_millis: int = Field(
        description="Execution time in milliseconds", alias="execution_time_in_millis"
    )

    model_config = ConfigDict(populate_by_name=True)


class ExecutionInit(BaseModel):
    """
    Execution initialization event.

    Represents the initialization of a code execution.
    """

    id: str = Field(description="Execution identifier")
    timestamp: int = Field(description="Unix timestamp when execution started")

    model_config = ConfigDict(populate_by_name=True)


class Execution(BaseModel):
    """
    Complete code execution session.

    This is the main model that tracks the entire lifecycle of code execution,
    including results, errors, and output logs. It serves as the central container
    for all execution-related data that is exposed to users.
    """

    id: str | None = Field(default=None, description="Unique execution identifier")
    execution_count: int | None = Field(
        default=None,
        description="Sequential execution counter",
        alias="execution_count",
    )
    result: list["ExecutionResult"] = Field(
        default_factory=list, description="Execution results"
    )
    error: ExecutionError | None = Field(
        default=None, description="Error information if failed"
    )
    complete: ExecutionComplete | None = Field(
        default=None, description="Completion metadata if execution completed"
    )
    exit_code: int | None = Field(
        default=None,
        description="Command exit code when available",
        alias="exit_code",
    )
    logs: ExecutionLogs = Field(
        default_factory=ExecutionLogs, description="Output logs"
    )

    def add_result(self, result: ExecutionResult) -> None:
        """Add a new execution result."""
        self.result.append(result)

    @property
    def text(self) -> str:
        """Return combined stdout and result text.

        Includes both stdout log messages and execution results,
        stripping trailing newlines from each chunk to avoid double
        line breaks when messages already contain trailing newlines
        (e.g. code-interpreter streaming output).
        """
        chunks: list[str] = []

        for msg in self.logs.stdout:
            chunks.append(msg.text.rstrip("\n"))

        for res in self.result:
            if res.text:
                chunks.append(res.text.rstrip("\n"))

        return "\n".join(chunks)

    def __str__(self) -> str:
        """Return a human-readable summary of the execution."""
        parts: list[str] = []

        if self.logs.stdout or self.result:
            parts.append(self.text)

        if self.logs.stderr:
            stderr_text = "\n".join(msg.text.rstrip("\n") for msg in self.logs.stderr)
            parts.append(f"[stderr]\n{stderr_text}")

        if self.error:
            parts.append(f"[error] {self.error.name}: {self.error.value}")

        return "\n".join(parts)

    model_config = ConfigDict(populate_by_name=True)


# Type aliases for async handlers
AsyncOutputHandler = Callable[[Any], Awaitable[None]]


class ExecutionHandlers(BaseModel):
    """
    Async handlers for code execution output processing.

    Provides optional async callback handlers for different types of execution events.
    All handlers are async functions that will be awaited when events occur.

    Example:
        ```python
        async def handle_stdout(msg: OutputMessage):
            print(f"Output: {msg.text}")
            # Can perform async operations
            await log_to_database(msg.text)

        handlers = ExecutionHandlers(
            on_stdout=handle_stdout,
            on_stderr=lambda msg: print(f"Error: {msg.text}"),
        )
        ```
    """

    on_stdout: AsyncOutputHandler | None = Field(
        default=None, description="Async handler for stdout messages"
    )
    on_stderr: AsyncOutputHandler | None = Field(
        default=None, description="Async handler for stderr messages"
    )
    on_result: AsyncOutputHandler | None = Field(
        default=None, description="Async handler for execution results"
    )
    on_execution_complete: AsyncOutputHandler | None = Field(
        default=None,
        description="Async handler for execution completion",
        alias="on_execution_complete",
    )
    on_error: AsyncOutputHandler | None = Field(
        default=None, description="Async handler for execution errors"
    )
    on_init: AsyncOutputHandler | None = Field(
        default=None, description="Async handler for execution init"
    )

    model_config = ConfigDict(populate_by_name=True, arbitrary_types_allowed=True)


class RunCommandOpts(BaseModel):
    """
    Parameters for command execution.
    """

    background: bool = Field(
        default=False, description="Whether to run in background (detached)"
    )
    working_directory: str | None = Field(
        default=None,
        description="Directory to execute command in",
        alias="working_directory",
    )
    timeout: timedelta | None = Field(
        default=None,
        description="Maximum execution time; server will terminate the command when reached. If omitted, the server will not enforce any timeout.",
    )
    uid: int | None = Field(
        default=None,
        ge=0,
        description="Unix user ID used to run the command process.",
    )
    gid: int | None = Field(
        default=None,
        ge=0,
        description="Unix group ID used to run the command process. Requires uid to be set.",
    )
    envs: dict[str, str] | None = Field(
        default=None,
        description="Environment variables injected into the command process.",
    )

    @model_validator(mode="after")
    def validate_uid_gid_dependency(self) -> "RunCommandOpts":
        """Ensure gid is not used without uid to match server contract."""
        if self.gid is not None and self.uid is None:
            raise ValueError("uid is required when gid is provided")
        return self

    model_config = ConfigDict(populate_by_name=True, arbitrary_types_allowed=True)


class CommandStatus(BaseModel):
    """
    Command execution status for foreground/background commands.
    """

    id: str | None = Field(default=None, description="Command ID")
    content: str | None = Field(default=None, description="Original command content")
    running: bool | None = Field(
        default=None, description="True if command is still running"
    )
    exit_code: int | None = Field(
        default=None, description="Exit code if the command has finished"
    )
    error: str | None = Field(
        default=None, description="Error message if the command failed"
    )
    started_at: datetime | None = Field(
        default=None, description="Command start time (RFC3339)", alias="started_at"
    )
    finished_at: datetime | None = Field(
        default=None, description="Command finish time (RFC3339)", alias="finished_at"
    )

    model_config = ConfigDict(populate_by_name=True)


class CommandLogs(BaseModel):
    """
    Background command logs with optional tail cursor for incremental reads.
    """

    content: str = Field(description="Raw stdout/stderr content")
    cursor: int | None = Field(
        default=None,
        description="Latest tail cursor for incremental reads",
    )
