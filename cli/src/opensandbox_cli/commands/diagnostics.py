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

"""Stable sandbox diagnostics commands backed by the Python SDK."""

from __future__ import annotations

from typing import Any

import click
from opensandbox.models.diagnostics import DiagnosticContent

from opensandbox_cli.client import ClientContext
from opensandbox_cli.utils import handle_errors, output_option, prepare_output


def _diagnostic_to_dict(content: DiagnosticContent) -> dict[str, Any]:
    """Convert SDK diagnostics content to a CLI-friendly dict."""
    return content.model_dump(mode="json")


def render_diagnostic_content(
    obj: ClientContext,
    content: DiagnosticContent,
    output_format: str | None,
    *,
    title: str,
) -> None:
    """Render diagnostics descriptor or raw diagnostic payload."""
    output = prepare_output(
        obj,
        output_format,
        allowed=("table", "json", "yaml", "raw"),
        fallback="raw",
    )

    if output.fmt == "raw":
        if content.delivery == "inline":
            click.echo(content.content or "")
            return
        if content.content_url:
            click.echo(content.content_url)
            return
        raise click.ClickException("Diagnostic response did not include inline content or a content URL.")

    output.print_dict(_diagnostic_to_dict(content), title=title)


@click.group("diagnostics", invoke_without_command=True)
@click.pass_context
def diagnostics_group(ctx: click.Context) -> None:
    """Stable sandbox diagnostics backed by the OpenSandbox SDK."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


@diagnostics_group.command("logs")
@click.argument("sandbox_id")
@click.option(
    "--scope",
    "-s",
    required=True,
    help=(
        "Diagnostic log scope. Common scopes: lifecycle for manager logs, "
        "container for sandbox stdout; other scopes are server-defined."
    ),
)
@output_option(
    "table",
    "json",
    "yaml",
    "raw",
    help_text="Output format. raw prints inline text, or the content URL for URL-delivered diagnostics.",
)
@click.pass_obj
@handle_errors
def diagnostics_logs(
    obj: ClientContext,
    sandbox_id: str,
    scope: str,
    output_format: str | None,
) -> None:
    """Retrieve diagnostic logs for a sandbox."""
    sandbox_id = obj.resolve_sandbox_id(sandbox_id)
    content = obj.get_manager().get_diagnostic_logs(sandbox_id, scope=scope)
    render_diagnostic_content(obj, content, output_format, title="Diagnostic Logs")


@diagnostics_group.command("events")
@click.argument("sandbox_id")
@click.option(
    "--scope",
    "-s",
    required=True,
    help=(
        "Diagnostic event scope. Common scopes: lifecycle for audit events, "
        "runtime for scheduler/container events; other scopes are server-defined."
    ),
)
@output_option(
    "table",
    "json",
    "yaml",
    "raw",
    help_text="Output format. raw prints inline text, or the content URL for URL-delivered diagnostics.",
)
@click.pass_obj
@handle_errors
def diagnostics_events(
    obj: ClientContext,
    sandbox_id: str,
    scope: str,
    output_format: str | None,
) -> None:
    """Retrieve diagnostic events for a sandbox."""
    sandbox_id = obj.resolve_sandbox_id(sandbox_id)
    content = obj.get_manager().get_diagnostic_events(sandbox_id, scope=scope)
    render_diagnostic_content(obj, content, output_format, title="Diagnostic Events")
