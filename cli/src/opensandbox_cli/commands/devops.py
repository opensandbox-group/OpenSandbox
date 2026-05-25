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

"""Experimental DevOps diagnostics commands: logs, inspect, events, summary."""

from __future__ import annotations

import click

from opensandbox_cli.client import ClientContext
from opensandbox_cli.utils import handle_errors, output_option, prepare_output


def _fetch_plain_text(obj: ClientContext, sandbox_id: str, endpoint: str, params: dict | None = None) -> str:
    """Fetch a diagnostics endpoint and return the plain-text body."""
    sandbox_id = obj.resolve_sandbox_id(sandbox_id)
    client = obj.get_devops_client()
    resp = client.get(f"sandboxes/{sandbox_id}/diagnostics/{endpoint}", params=params)
    if resp.status_code == 404:
        raise click.ClickException(f"Sandbox '{sandbox_id}' not found.")
    resp.raise_for_status()
    return resp.text


@click.group("devops", invoke_without_command=True)
@click.pass_context
def devops_group(ctx: click.Context) -> None:
    """Experimental diagnostics for sandbox troubleshooting."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


# ---- logs ----------------------------------------------------------------

@devops_group.command("logs")
@click.argument("sandbox_id")
@click.option("--tail", "-n", type=int, default=100, show_default=True, help="Number of trailing log lines.")
@click.option("--since", "-s", default=None, help="Only logs newer than this duration (e.g. 10m, 1h).")
@output_option("raw", help_text="Output format: raw.")
@click.pass_obj
@handle_errors
def devops_logs(
    obj: ClientContext,
    sandbox_id: str,
    tail: int,
    since: str | None,
    output_format: str | None,
) -> None:
    """Deprecated: use `osb diagnostics logs` instead."""
    click.echo("Warning: `osb devops logs` is deprecated. Use `osb diagnostics logs`.", err=True)
    prepare_output(obj, output_format, allowed=("raw",), fallback="raw")
    params: dict = {"tail": tail}
    if since:
        params["since"] = since
    text = _fetch_plain_text(obj, sandbox_id, "logs", params=params)
    click.echo(text)


# ---- inspect -------------------------------------------------------------

@devops_group.command("inspect")
@click.argument("sandbox_id")
@output_option("raw", help_text="Output format: raw.")
@click.pass_obj
@handle_errors
def devops_inspect(
    obj: ClientContext, sandbox_id: str, output_format: str | None
) -> None:
    """Retrieve detailed container/pod inspection info."""
    prepare_output(obj, output_format, allowed=("raw",), fallback="raw")
    text = _fetch_plain_text(obj, sandbox_id, "inspect")
    click.echo(text)


# ---- events --------------------------------------------------------------

@devops_group.command("events")
@click.argument("sandbox_id")
@click.option("--limit", "-l", type=int, default=50, show_default=True, help="Maximum number of events.")
@output_option("raw", help_text="Output format: raw.")
@click.pass_obj
@handle_errors
def devops_events(
    obj: ClientContext, sandbox_id: str, limit: int, output_format: str | None
) -> None:
    """Deprecated: use `osb diagnostics events` instead."""
    click.echo("Warning: `osb devops events` is deprecated. Use `osb diagnostics events`.", err=True)
    prepare_output(obj, output_format, allowed=("raw",), fallback="raw")
    params: dict = {"limit": limit}
    text = _fetch_plain_text(obj, sandbox_id, "events", params=params)
    click.echo(text)


# ---- summary -------------------------------------------------------------

@devops_group.command("summary")
@click.argument("sandbox_id")
@click.option("--tail", "-n", type=int, default=50, show_default=True, help="Number of trailing log lines.")
@click.option("--event-limit", type=int, default=20, show_default=True, help="Maximum number of events.")
@output_option("raw", help_text="Output format: raw.")
@click.pass_obj
@handle_errors
def devops_summary(
    obj: ClientContext,
    sandbox_id: str,
    tail: int,
    event_limit: int,
    output_format: str | None,
) -> None:
    """One-shot diagnostics: inspect + events + logs combined."""
    prepare_output(obj, output_format, allowed=("raw",), fallback="raw")
    params: dict = {"tail": tail, "event_limit": event_limit}
    text = _fetch_plain_text(obj, sandbox_id, "summary", params=params)
    click.echo(text)
