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

"""Command execution commands: one interface, foreground stream or background tracking."""

from __future__ import annotations

import shlex
import sys
from datetime import timedelta

import click
from opensandbox.models.execd import OutputMessage, RunCommandOpts
from opensandbox.models.execd_sync import ExecutionHandlersSync

from opensandbox_cli.client import ClientContext
from opensandbox_cli.utils import (
    DURATION,
    handle_errors,
    output_option,
    prepare_output,
)


@click.group("command", invoke_without_command=True)
@click.pass_context
def command_group(ctx: click.Context) -> None:
    """⚡ Execute commands in a sandbox."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


# ---- run ------------------------------------------------------------------

def _run_command(
    obj: ClientContext,
    sandbox_id: str,
    command: tuple[str, ...],
    background: bool,
    workdir: str | None,
    timeout: timedelta | None,
    output_format: str | None,
) -> None:
    """Shared implementation for ``command run``.

    Mode contract:
    - foreground (default): stream output directly, allow only ``-o raw``
    - background (``--background``): return a tracked execution object, allow structured output
    """
    allowed = ("table", "json", "yaml") if background else ("raw",)
    fallback = "table" if background else "raw"
    prepare_output(obj, output_format, allowed=allowed, fallback=fallback)
    cmd_str = " ".join(shlex.quote(arg) for arg in command)
    sandbox = obj.connect_sandbox(sandbox_id)

    try:
        opts = RunCommandOpts(
            background=background,
            working_directory=workdir,
            timeout=timeout,
        )

        if background:
            execution = sandbox.commands.run(cmd_str, opts=opts)
            obj.output.success_panel(
                {
                    "execution_id": execution.id,
                    "sandbox_id": sandbox_id,
                    "mode": "background",
                },
                title="Background Command Started",
            )
            return

        # Foreground: stream stdout/stderr to terminal
        last_text = ""

        def on_stdout(msg: OutputMessage) -> None:
            nonlocal last_text
            last_text = msg.text
            sys.stdout.write(msg.text)
            sys.stdout.flush()

        def on_stderr(msg: OutputMessage) -> None:
            nonlocal last_text
            last_text = msg.text
            sys.stderr.write(msg.text)
            sys.stderr.flush()

        handlers = ExecutionHandlersSync(on_stdout=on_stdout, on_stderr=on_stderr)
        execution = sandbox.commands.run(cmd_str, opts=opts, handlers=handlers)

        # Ensure terminal prompt starts on a new line
        if last_text and not last_text.endswith("\n"):
            sys.stdout.write("\n")
            sys.stdout.flush()

        _handle_execution_error(obj, execution)
    finally:
        sandbox.close()


def _handle_execution_error(obj: ClientContext, execution) -> None:
    """Exit non-zero if the execution finished with an error."""
    if execution.error:
        obj.output.error_panel(
            f"{execution.error.name}: {execution.error.value}",
            title="Execution Error",
        )
        sys.exit(1)


@command_group.command(
    "run",
    help="Run a command in a sandbox. Use `--` before the sandbox command payload.",
    epilog="Separator rule: use `--` before the sandbox command payload.",
)
@click.argument("sandbox_id")
@click.argument("command", nargs=-1, required=True)
@click.option("-d", "--background", is_flag=True, default=False, help="Run in background.")
@click.option("-w", "--workdir", default=None, help="Working directory.")
@click.option("-t", "--timeout", type=DURATION, default=None, help="Command timeout (e.g. 30s, 5m).")
@output_option("table", "json", "yaml", "raw")
@click.pass_obj
@handle_errors
def command_run(
    obj: ClientContext,
    sandbox_id: str,
    command: tuple[str, ...],
    background: bool,
    workdir: str | None,
    timeout: timedelta | None,
    output_format: str | None,
) -> None:
    """Run a command in a sandbox.

    Default mode streams output directly. Add ``--background`` to return a
    tracked execution object instead. Use ``--`` before the sandbox command payload.
    """
    _run_command(
        obj,
        sandbox_id,
        command,
        background,
        workdir,
        timeout,
        output_format,
    )


# ---- status ---------------------------------------------------------------

@command_group.command("status")
@click.argument("sandbox_id")
@click.argument("execution_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def command_status(
    obj: ClientContext,
    sandbox_id: str,
    execution_id: str,
    output_format: str | None,
) -> None:
    """Get command execution status."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        status = sandbox.commands.get_command_status(execution_id)
        obj.output.print_model(status, title="Command Status")
    finally:
        sandbox.close()


# ---- logs -----------------------------------------------------------------

@command_group.command("logs")
@click.argument("sandbox_id")
@click.argument("execution_id")
@click.option("--cursor", type=int, default=None, help="Cursor for incremental reads.")
@output_option("table", "json", "yaml", "raw")
@click.pass_obj
@handle_errors
def command_logs(
    obj: ClientContext,
    sandbox_id: str,
    execution_id: str,
    cursor: int | None,
    output_format: str | None,
) -> None:
    """Get background command logs."""
    prepare_output(
        obj, output_format, allowed=("table", "json", "yaml", "raw"), fallback="table"
    )
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        logs = sandbox.commands.get_background_command_logs(execution_id, cursor=cursor)
        if obj.output.fmt in ("json", "yaml"):
            obj.output.print_model(logs, title="Command Logs")
        elif obj.output.fmt == "raw":
            click.echo(logs.content)
        else:
            obj.output.panel(logs.content, title="Command Logs")
    finally:
        sandbox.close()


# ---- interrupt ------------------------------------------------------------

@command_group.command("interrupt")
@click.argument("sandbox_id")
@click.argument("execution_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def command_interrupt(
    obj: ClientContext,
    sandbox_id: str,
    execution_id: str,
    output_format: str | None,
) -> None:
    """Interrupt a running command."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        sandbox.commands.interrupt(execution_id)
        obj.output.success(f"Interrupted: {execution_id}")
    finally:
        sandbox.close()


@command_group.group("session", invoke_without_command=True)
@click.pass_context
def session_group(ctx: click.Context) -> None:
    """Manage persistent bash sessions."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


@session_group.command("create")
@click.argument("sandbox_id")
@click.option("-w", "--workdir", default=None, help="Initial working directory.")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def session_create(
    obj: ClientContext,
    sandbox_id: str,
    workdir: str | None,
    output_format: str | None,
) -> None:
    """Create a persistent bash session."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        session_id = sandbox.commands.create_session(working_directory=workdir)
        obj.output.success_panel(
            {
                "sandbox_id": sandbox.id,
                "session_id": session_id,
                "working_directory": workdir,
            },
            title="Session Created",
        )
    finally:
        sandbox.close()


@session_group.command(
    "run",
    help="Run a command in an existing bash session. Use `--` before the sandbox command payload.",
    epilog="Separator rule: use `--` before the sandbox command payload.",
)
@click.argument("sandbox_id")
@click.argument("session_id")
@click.argument("command", nargs=-1, required=True)
@click.option("-w", "--workdir", default=None, help="Working directory override for this run.")
@click.option("-t", "--timeout", type=DURATION, default=None, help="Command timeout (e.g. 30s, 5m).")
@output_option("raw", help_text="Output format: raw.")
@click.pass_obj
@handle_errors
def session_run(
    obj: ClientContext,
    sandbox_id: str,
    session_id: str,
    command: tuple[str, ...],
    workdir: str | None,
    timeout: timedelta | None,
    output_format: str | None,
) -> None:
    """Run a command in an existing bash session. Use `--` before the sandbox command payload."""
    prepare_output(obj, output_format, allowed=("raw",), fallback="raw")
    cmd_str = " ".join(shlex.quote(arg) for arg in command)
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        last_text = ""

        def on_stdout(msg: OutputMessage) -> None:
            nonlocal last_text
            last_text = msg.text
            sys.stdout.write(msg.text)
            sys.stdout.flush()

        def on_stderr(msg: OutputMessage) -> None:
            nonlocal last_text
            last_text = msg.text
            sys.stderr.write(msg.text)
            sys.stderr.flush()

        handlers = ExecutionHandlersSync(on_stdout=on_stdout, on_stderr=on_stderr)
        execution = sandbox.commands.run_in_session(
            session_id,
            cmd_str,
            working_directory=workdir,
            timeout=timeout,
            handlers=handlers,
        )

        if last_text and not last_text.endswith("\n"):
            sys.stdout.write("\n")
            sys.stdout.flush()

        _handle_execution_error(obj, execution)
    finally:
        sandbox.close()


@session_group.command("delete")
@click.argument("sandbox_id")
@click.argument("session_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def session_delete(
    obj: ClientContext,
    sandbox_id: str,
    session_id: str,
    output_format: str | None,
) -> None:
    """Delete a persistent bash session."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        sandbox.commands.delete_session(session_id)
        obj.output.success(f"Deleted session: {session_id}")
    finally:
        sandbox.close()

