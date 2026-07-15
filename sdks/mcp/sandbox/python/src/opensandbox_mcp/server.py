# Copyright 2026 Alibaba Group Holding Ltd.
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

from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from datetime import timedelta

from mcp.server.fastmcp import Context, FastMCP
from mcp.server.session import ServerSession
from opensandbox import Sandbox, SandboxManager
from opensandbox.config import ConnectionConfig
from opensandbox.models.execd import Execution, RunCommandOpts
from opensandbox.models.filesystem import (
    ContentReplaceEntry,
    ContentReplaceResult,
    EntryInfo,
    MoveEntry,
    SearchEntry,
    WriteEntry,
)
from opensandbox.models.sandboxes import (
    NetworkPolicy,
    PagedSandboxInfos,
    SandboxEndpoint,
    SandboxFilter,
    SandboxImageAuth,
    SandboxImageSpec,
    SandboxInfo,
    SandboxMetrics,
    SandboxRenewResponse,
)
from pydantic import BaseModel, Field


@dataclass
class ServerState:
    sandboxes: dict[str, Sandbox] = field(default_factory=dict)
    connection_config: ConnectionConfig = field(default_factory=ConnectionConfig)
    lock: asyncio.Lock = field(default_factory=asyncio.Lock)

    async def add(self, sandbox: Sandbox) -> None:
        async with self.lock:
            self.sandboxes[sandbox.id] = sandbox

    async def get(self, sandbox_id: str) -> Sandbox | None:
        async with self.lock:
            return self.sandboxes.get(sandbox_id)

    async def remove(self, sandbox_id: str) -> Sandbox | None:
        async with self.lock:
            return self.sandboxes.pop(sandbox_id, None)


class StatusResponse(BaseModel):
    status: str = Field(description="Operation status string.")

class DirectoryEntryInput(BaseModel):
    path: str = Field(description="Directory path.")
    mode: int = Field(default=755, description="Unix permissions for the directory.")
    owner: str | None = Field(default=None, description="Owner username.")
    group: str | None = Field(default=None, description="Group name.")

class SandboxInfoResponse(BaseModel):
    sandbox_id: str = Field(description="Sandbox identifier.")
    info: SandboxInfo = Field(description="Sandbox info payload.")

class SandboxHealthResponse(BaseModel):
    sandbox_id: str = Field(description="Sandbox identifier.")
    healthy: bool = Field(description="Sandbox health status.")

class FileReadResponse(BaseModel):
    path: str = Field(description="File path.")
    content: str = Field(description="File content.")


def register_tools(
    mcp: FastMCP,
    *,
    prefix: str = "",
    state: ServerState | None = None,
    connection_config: ConnectionConfig | None = None,
) -> ServerState:
    """Register sandbox tools on a FastMCP instance."""
    config = (connection_config or ConnectionConfig()).with_transport_if_missing()
    state = state or ServerState(connection_config=config)
    name_prefix = f"{prefix}_" if prefix else ""

    def tool():
        def decorator(func):
            if name_prefix:
                func.__name__ = f"{name_prefix}{func.__name__}"
            return mcp.tool()(func)

        return decorator

    async def _get_or_connect_sandbox(
        sandbox_id: str,
        *,
        connect_if_missing: bool,
    ) -> Sandbox:
        sandbox = await state.get(sandbox_id)
        if sandbox is not None:
            return sandbox
        if not connect_if_missing:
            raise ValueError(
                "Sandbox not found in local registry. Call sandbox_connect or "
                "set connect_if_missing=True with connection parameters."
            )
        sandbox = await Sandbox.connect(
            sandbox_id, connection_config=state.connection_config
        )
        await state.add(sandbox)
        return sandbox

    @tool()
    async def sandbox_create(
        image: str,
        ctx: Context[ServerSession, None] | None = None,
        *,
        auth_username: str | None = None,
        auth_password: str | None = None,
        timeout_seconds: float = 600,
        ready_timeout_seconds: float = 30,
        health_check_polling_interval_ms: int = 200,
        skip_health_check: bool = False,
        env: dict[str, str] | None = None,
        metadata: dict[str, str] | None = None,
        resource: dict[str, str] | None = None,
        network_policy: NetworkPolicy | None = None,
        extensions: dict[str, str] | None = None,
        entrypoint: list[str] | None = None,
    ) -> SandboxInfoResponse:
        """Create a sandbox and store it in the MCP server session.

        This allocates a new sandbox instance using the OpenSandbox API and
        tracks it locally so subsequent tool calls can reuse it.

        Parameters:
            image: Container image reference (e.g., "python:3.11").
            ctx: MCP context for progress reporting (optional).
            auth_username: Registry username for private images.
            auth_password: Registry password/token for private images.
            timeout_seconds: Sandbox lifetime in seconds (absolute TTL).
            ready_timeout_seconds: Max time to wait for readiness checks.
            health_check_polling_interval_ms: Interval between health checks in ms.
            skip_health_check: If True, return before readiness checks complete.
            env: Environment variables for the sandbox.
            metadata: Custom metadata for the sandbox (string map).
            resource: Resource limits (cpu/memory/etc.) as string map.
            network_policy: Optional egress network policy (NetworkPolicy model).
                Example: NetworkPolicy(
                    default_action="deny",
                    egress=[{"action": "allow", "target": "pypi.org"}],
                )
            extensions: Opaque extension parameters passed through to the server.
            entrypoint: Entrypoint command list.

        Returns:
            A dict with:
                sandbox_id: The new sandbox identifier.
                info: Sandbox info payload from the SDK.

        Raises:
            ValueError: If auth_username/auth_password are incomplete.
            Exception: If sandbox creation fails.

        Example:
            result = await sandbox_create(
                image="python:3.11",
                env={"PYTHONPATH": "/app"},
                resource={"cpu": "1", "memory": "2Gi"},
            )
        """
        if ctx:
            await ctx.report_progress(progress=0.1, total=1.0, message="Validating input")
        image_auth = None
        if auth_username or auth_password:
            if not auth_username or not auth_password:
                raise ValueError("auth_username and auth_password must be provided together")
            image_auth = SandboxImageAuth(
                username=auth_username,
                password=auth_password,
            )
        image_spec = SandboxImageSpec(image=image, auth=image_auth)
        if ctx:
            await ctx.report_progress(
                progress=0.3, total=1.0, message="Creating sandbox"
            )
        sandbox = await Sandbox.create(
            image_spec,
            timeout=timedelta(seconds=timeout_seconds),
            ready_timeout=timedelta(seconds=ready_timeout_seconds),
            env=env,
            metadata=metadata,
            resource=resource,
            network_policy=network_policy,
            extensions=extensions,
            entrypoint=entrypoint,
            health_check_polling_interval=timedelta(
                milliseconds=health_check_polling_interval_ms
            ),
            skip_health_check=skip_health_check,
            connection_config=state.connection_config,
        )
        await state.add(sandbox)
        if ctx:
            await ctx.report_progress(
                progress=0.8, total=1.0, message="Fetching sandbox info"
            )
        info = await sandbox.get_info()
        if ctx:
            await ctx.report_progress(progress=1.0, total=1.0, message="Done")
        return SandboxInfoResponse(sandbox_id=sandbox.id, info=info)

    @tool()
    async def sandbox_connect(
        sandbox_id: str,
        *,
        connect_timeout_seconds: float = 30,
        health_check_polling_interval_ms: int = 200,
        skip_health_check: bool = False,
    ) -> SandboxInfoResponse:
        """Connect to an existing sandbox and store it locally.

        Use this when a sandbox already exists and you want to use it in this
        MCP server session without creating a new one.

        Parameters:
            sandbox_id: Existing sandbox identifier.
            connect_timeout_seconds: Max time to wait for readiness.
            health_check_polling_interval_ms: Interval between health checks in ms.
            skip_health_check: If True, return before readiness checks complete.

        Returns:
            A dict with:
                sandbox_id: The sandbox identifier.
                info: Sandbox info payload from the SDK.

        Example:
            result = await sandbox_connect(sandbox_id="sbx_123")
        """
        sandbox = await Sandbox.connect(
            sandbox_id,
            connection_config=state.connection_config,
            connect_timeout=timedelta(seconds=connect_timeout_seconds),
            health_check_polling_interval=timedelta(
                milliseconds=health_check_polling_interval_ms
            ),
            skip_health_check=skip_health_check,
        )
        await state.add(sandbox)
        info = await sandbox.get_info()
        return SandboxInfoResponse(sandbox_id=sandbox.id, info=info)

    @tool()
    async def sandbox_kill(
        sandbox_id: str,
    ) -> StatusResponse:
        """Terminate a sandbox by ID and remove it from local registry.

        Parameters:
            sandbox_id: Target sandbox identifier.

        Returns:
            {"status": "killed"} when successful.
        """
        sandbox = await state.remove(sandbox_id)
        if sandbox is None:
            manager = await SandboxManager.create(
                connection_config=state.connection_config
            )
            try:
                await manager.kill_sandbox(sandbox_id)
            finally:
                await manager.close()
        else:
            try:
                await sandbox.kill()
            finally:
                await sandbox.close()
        return StatusResponse(status="killed")

    @tool()
    async def sandbox_get_info(
        sandbox_id: str,
    ) -> SandboxInfo:
        """Fetch sandbox info by ID.

        Parameters:
            sandbox_id: Target sandbox identifier.

        Returns:
            Sandbox info dict from the SDK.
        """
        sandbox = await state.get(sandbox_id)
        if sandbox is not None:
            return await sandbox.get_info()
        manager = await SandboxManager.create(
            connection_config=state.connection_config
        )
        try:
            info = await manager.get_sandbox_info(sandbox_id)
        finally:
            await manager.close()
        return info

    @tool()
    async def sandbox_list(
        ctx: Context[ServerSession, None] | None = None,
        *,
        filter: SandboxFilter | None = None,
    ) -> PagedSandboxInfos:
        """List sandboxes matching filter criteria.

        Parameters:
            ctx: MCP context for progress reporting (optional).
            filter: SandboxFilter object (states, metadata, page, page_size).

        Returns:
            Paginated sandbox list.
        """
        if ctx:
            await ctx.report_progress(progress=0.1, total=1.0, message="Listing sandboxes")
        filter = filter or SandboxFilter()
        manager = await SandboxManager.create(
            connection_config=state.connection_config
        )
        try:
            result = await manager.list_sandbox_infos(filter)
        finally:
            await manager.close()
        if ctx:
            await ctx.report_progress(progress=1.0, total=1.0, message="Done")
        return result

    @tool()
    async def sandbox_renew(
        sandbox_id: str,
        *,
        timeout_seconds: float,
    ) -> SandboxRenewResponse:
        """Renew sandbox expiration time.

        Parameters:
            sandbox_id: Target sandbox identifier.
            timeout_seconds: Additional lifetime in seconds.

        Returns:
            Renew response dict including new expiration time.
        """
        sandbox = await state.get(sandbox_id)
        if sandbox is None:
            manager = await SandboxManager.create(
                connection_config=state.connection_config
            )
            try:
                response = await manager.renew_sandbox(
                    sandbox_id, timedelta(seconds=timeout_seconds)
                )
            finally:
                await manager.close()
        else:
            response = await sandbox.renew(timedelta(seconds=timeout_seconds))
        return response

    @tool()
    async def sandbox_get_metrics(
        sandbox_id: str,
        *,
        connect_if_missing: bool = False,
    ) -> SandboxMetrics:
        """Get resource metrics for a sandbox.

        Parameters:
            sandbox_id: Target sandbox identifier.
            connect_if_missing: Connect if sandbox not in local registry.

        Returns:
            Metrics dict.
        """
        sandbox = await _get_or_connect_sandbox(
            sandbox_id,
            connect_if_missing=connect_if_missing,
        )
        metrics = await sandbox.get_metrics()
        return metrics

    @tool()
    async def sandbox_healthcheck(
        sandbox_id: str,
        *,
        connect_if_missing: bool = False,
    ) -> SandboxHealthResponse:
        """Check if a sandbox is healthy.

        Parameters:
            sandbox_id: Target sandbox identifier.
            connect_if_missing: Connect if sandbox not in local registry.

        Returns:
            {"sandbox_id": "...", "healthy": true|false}.
        """
        sandbox = await _get_or_connect_sandbox(
            sandbox_id,
            connect_if_missing=connect_if_missing,
        )
        healthy = await sandbox.is_healthy()
        return SandboxHealthResponse(sandbox_id=sandbox_id, healthy=healthy)

    @tool()
    async def command_run(
        sandbox_id: str,
        command: str,
        *,
        background: bool = False,
        working_directory: str | None = None,
        connect_if_missing: bool = False,
    ) -> Execution:
        """Run a command inside a sandbox.
        Parameters:
            sandbox_id: Target sandbox identifier.
            command: Shell command to execute (supports pipes/redirects).
            background: If True, run asynchronously and return immediately.
            working_directory: Working directory for the command.
            connect_if_missing: Connect if sandbox not in local registry.

        Returns:
            Execution result dict with id, exit_code, logs, and duration.

        Example:
            result = await command_run("sbx_123", "ls -la", working_directory="/")
        """
        sandbox = await _get_or_connect_sandbox(
            sandbox_id,
            connect_if_missing=connect_if_missing,
        )
        opts = RunCommandOpts(
            background=background,
            working_directory=working_directory,
        )
        execution = await sandbox.commands.run(command, opts=opts)
        return execution

    @tool()
    async def command_interrupt(
        sandbox_id: str,
        execution_id: str,
        *,
        connect_if_missing: bool = False,
    ) -> StatusResponse:
        """Interrupt a running command execution.

        Parameters:
            sandbox_id: Target sandbox identifier.
            execution_id: Execution identifier to interrupt.
            connect_if_missing: Connect if sandbox not in local registry.

        Returns:
            {"status": "interrupted"} when successful.
        """
        sandbox = await _get_or_connect_sandbox(
            sandbox_id,
            connect_if_missing=connect_if_missing,
        )
        await sandbox.commands.interrupt(execution_id)
        return StatusResponse(status="interrupted")

    @tool()
    async def file_read(
        sandbox_id: str,
        path: str,
        *,
        encoding: str = "utf-8",
        range_header: str | None = None,
        connect_if_missing: bool = False,
    ) -> FileReadResponse:
        """Read a text file from the sandbox.

        Parameters:
            sandbox_id: Target sandbox identifier.
            path: File path to read.
            encoding: Text encoding.
            range_header: Optional byte range header (e.g., "bytes=0-1023").
            connect_if_missing: Connect if sandbox not in local registry.

        Returns:
            {"path": "...", "content": "..."}.

        """
        sandbox = await _get_or_connect_sandbox(
            sandbox_id,
            connect_if_missing=connect_if_missing,
        )
        content = await sandbox.files.read_file(
            path, encoding=encoding, range_header=range_header
        )
        return FileReadResponse(path=path, content=content)

    @tool()
    async def file_write(
        sandbox_id: str,
        path: str,
        content: str,
        *,
        encoding: str = "utf-8",
        mode: int = 755,
        owner: str | None = None,
        group: str | None = None,
        connect_if_missing: bool = False,
    ) -> StatusResponse:
        """Write a text file inside the sandbox.

        Parameters:
            sandbox_id: Target sandbox identifier.
            path: Destination file path.
            content: File content.
            encoding: Text encoding.
            mode: Unix file permissions.
            owner: Owner username.
            group: Group name.
            connect_if_missing: Connect if sandbox not in local registry.

        Returns:
            {"status": "written"} when successful.
        """
        sandbox = await _get_or_connect_sandbox(
            sandbox_id,
            connect_if_missing=connect_if_missing,
        )
        await sandbox.files.write_file(
            path,
            content,
            encoding=encoding,
            mode=mode,
            owner=owner,
            group=group,
        )
        return StatusResponse(status="written")

    @tool()
    async def file_delete(
        sandbox_id: str,
        paths: list[str],
        *,
        connect_if_missing: bool = False,
    ) -> StatusResponse:
        """Delete files inside the sandbox.

        Parameters:
            sandbox_id: Target sandbox identifier.
            paths: File paths to delete.
            connect_if_missing: Connect if sandbox not in local registry.

        Returns:
            {"status": "deleted"} when successful.
        """
        sandbox = await _get_or_connect_sandbox(
            sandbox_id,
            connect_if_missing=connect_if_missing,
        )
        await sandbox.files.delete_files(paths)
        return StatusResponse(status="deleted")

    @tool()
    async def file_search(
        sandbox_id: str,
        path: str,
        pattern: str,
        *,
        connect_if_missing: bool = False,
    ) -> list[EntryInfo]:
        """Search for files matching a pattern.

        Parameters:
            sandbox_id: Target sandbox identifier.
            path: Base directory to search.
            pattern: Glob pattern (e.g., "*.py").
            connect_if_missing: Connect if sandbox not in local registry.

        Returns:
            List of entry info objects.
        """
        sandbox = await _get_or_connect_sandbox(
            sandbox_id,
            connect_if_missing=connect_if_missing,
        )
        results = await sandbox.files.search(SearchEntry(path=path, pattern=pattern))
        return results

    @tool()
    async def file_create_directories(
        sandbox_id: str,
        entries: list[DirectoryEntryInput],
        *,
        connect_if_missing: bool = False,
    ) -> StatusResponse:
        """Create directories inside the sandbox.

        Parameters:
            sandbox_id: Target sandbox identifier.
            entries: List of directory entries (path, mode, owner, group).
            connect_if_missing: Connect if sandbox not in local registry.

        Returns:
            {"status": "created"} when successful.
        """
        sandbox = await _get_or_connect_sandbox(
            sandbox_id,
            connect_if_missing=connect_if_missing,
        )
        write_entries = [
            WriteEntry(**entry.model_dump(exclude_none=True)) for entry in entries
        ]
        await sandbox.files.create_directories(write_entries)
        return StatusResponse(status="created")

    @tool()
    async def file_delete_directories(
        sandbox_id: str,
        paths: list[str],
        *,
        connect_if_missing: bool = False,
    ) -> StatusResponse:
        """Delete directories inside the sandbox.

        Parameters:
            sandbox_id: Target sandbox identifier.
            paths: Directory paths to delete.
            connect_if_missing: Connect if sandbox not in local registry.

        Returns:
            {"status": "deleted"} when successful.
        """
        sandbox = await _get_or_connect_sandbox(
            sandbox_id,
            connect_if_missing=connect_if_missing,
        )
        await sandbox.files.delete_directories(paths)
        return StatusResponse(status="deleted")

    @tool()
    async def file_move(
        sandbox_id: str,
        entries: list[MoveEntry],
        *,
        connect_if_missing: bool = False,
    ) -> StatusResponse:
        """Move or rename files/directories inside the sandbox.

        Parameters:
            sandbox_id: Target sandbox identifier.
            entries: List of move entries (source, destination).
            connect_if_missing: Connect if sandbox not in local registry.

        Returns:
            {"status": "moved"} when successful.
        """
        sandbox = await _get_or_connect_sandbox(
            sandbox_id,
            connect_if_missing=connect_if_missing,
        )
        await sandbox.files.move_files(entries)
        return StatusResponse(status="moved")

    @tool()
    async def file_replace_contents(
        sandbox_id: str,
        entries: list[ContentReplaceEntry],
        *,
        connect_if_missing: bool = False,
    ) -> list[ContentReplaceResult]:
        """Replace content inside files.

        Parameters:
            sandbox_id: Target sandbox identifier.
            entries: List of replace entries (path, old_content, new_content).
            connect_if_missing: Connect if sandbox not in local registry.

        Returns:
            List of replacement results with counts per file.
        """
        sandbox = await _get_or_connect_sandbox(
            sandbox_id,
            connect_if_missing=connect_if_missing,
        )
        replace_entries = [
            ContentReplaceEntry(**entry.model_dump(exclude_none=True))
            for entry in entries
        ]
        return await sandbox.files.replace_contents_detailed(replace_entries)

    @tool()
    async def sandbox_get_endpoint(
        sandbox_id: str,
        port: int,
        *,
        connect_if_missing: bool = False,
    ) -> SandboxEndpoint:
        """Get a sandbox network endpoint for a specific port.

        Parameters:
            sandbox_id: Target sandbox identifier.
            port: Port number inside the sandbox.
            connect_if_missing: Connect if sandbox not in local registry.

        Returns:
            Endpoint info dict.
        """
        sandbox = await _get_or_connect_sandbox(
            sandbox_id,
            connect_if_missing=connect_if_missing,
        )
        endpoint = await sandbox.get_endpoint(port)
        return endpoint

    return state


def create_server(connection_config: ConnectionConfig | None = None) -> FastMCP:
    """Create the MCP server instance for OpenSandbox."""
    mcp = FastMCP(
        "OpenSandbox Sandbox",
        instructions=(
            "Use these tools to create and manage isolated sandboxes. "
            "Always keep track of the sandbox_id returned by sandbox_create/connect. "
            "Use command_run for execution, file_read/file_write for file IO, and "
            "sandbox_kill to terminate remote sandboxes. Use sandbox_get_endpoint to "
            "expose sandbox ports; for large files, prefer range reads."
        ),
    )
    register_tools(mcp, connection_config=connection_config)
    return mcp
