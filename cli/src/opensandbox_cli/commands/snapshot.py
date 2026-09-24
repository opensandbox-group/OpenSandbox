# Copyright 2026 The OpenSandbox Authors.
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

"""Snapshot commands: create, get, list, delete."""

from __future__ import annotations

import click
from opensandbox.models.sandboxes import SnapshotFilter

from opensandbox_cli.utils import (
    handle_errors,
    output_option,
    prepare_output,
)


@click.group("snapshot", invoke_without_command=True)
@click.pass_context
def snapshot_group(ctx: click.Context) -> None:
    """📸 Manage sandbox snapshots."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


# ---- create ---------------------------------------------------------------

@snapshot_group.command("create")
@click.argument("sandbox_id")
@click.option("--name", default=None, help="Optional snapshot name.")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def snapshot_create(
    obj, sandbox_id: str, name: str | None, output_format: str | None
) -> None:
    """Create a snapshot from a sandbox."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox_id = obj.resolve_sandbox_id(sandbox_id)
    mgr = obj.get_manager()
    with obj.output.spinner("Creating snapshot..."):
        info = mgr.create_snapshot(sandbox_id, name=name)
    obj.output.success_panel(
        {
            "id": info.id,
            "sandbox_id": info.sandbox_id,
            "name": info.name,
            "status": info.status.state,
        },
        title="Snapshot Created",
    )


# ---- get ------------------------------------------------------------------

@snapshot_group.command("get")
@click.argument("snapshot_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def snapshot_get(obj, snapshot_id: str, output_format: str | None) -> None:
    """Get snapshot details."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    mgr = obj.get_manager()
    with obj.output.spinner("Fetching snapshot..."):
        info = mgr.get_snapshot(snapshot_id)
    d = info.model_dump(mode="json")

    if obj.output.fmt in ("json", "yaml"):
        obj.output.print_dict(d, title="Snapshot Info")
        return

    status_val = d.get("status")
    if isinstance(status_val, dict):
        d["status"] = status_val.get("state", str(status_val))
        if status_val.get("reason"):
            d["status_reason"] = status_val["reason"]
        if status_val.get("message"):
            d["status_message"] = status_val["message"]
    obj.output.print_dict(d, title="Snapshot Info")


# ---- list -----------------------------------------------------------------

@snapshot_group.command("list")
@click.option("--sandbox-id", "sandbox_id", default=None, help="Filter by source sandbox ID.")
@click.option("--name", default=None, help="Filter by exact snapshot name.")
@click.option("--state", "states", multiple=True, help="Filter by snapshot state. Repeatable.")
@click.option("--page", type=click.IntRange(min=1), default=None, help="Page number (1-indexed).")
@click.option("--page-size", type=click.IntRange(min=1), default=None, help="Items per page.")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def snapshot_list(
    obj,
    sandbox_id: str | None,
    name: str | None,
    states: tuple[str, ...],
    page: int | None,
    page_size: int | None,
    output_format: str | None,
) -> None:
    """List snapshots."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    mgr = obj.get_manager()
    filt = SnapshotFilter(
        sandbox_id=sandbox_id,
        name=name,
        states=list(states) if states else None,
        page=page,
        page_size=page_size,
    )
    with obj.output.spinner("Fetching snapshots..."):
        result = mgr.list_snapshots(filt)
    if not result.snapshot_infos:
        if obj.output.fmt in ("json", "yaml"):
            obj.output.print_dict(
                {
                    "items": [],
                    "pagination": result.pagination.model_dump(mode="json"),
                },
                title="Snapshots",
            )
        else:
            obj.output.info("No snapshots found.")
        return

    raw_rows = [info.model_dump(mode="json") for info in result.snapshot_infos]

    if obj.output.fmt in ("json", "yaml"):
        obj.output.print_dict(
            {
                "items": raw_rows,
                "pagination": result.pagination.model_dump(mode="json"),
            },
            title="Snapshots",
        )
        return

    rows = []
    for d in raw_rows:
        flat = dict(d)
        status_val = flat.get("status")
        if isinstance(status_val, dict):
            flat["status"] = status_val.get("state", str(status_val))
        rows.append(flat)

    obj.output.print_rows(
        rows,
        columns=["id", "name", "sandbox_id", "status", "created_at"],
        title="Snapshots",
    )


# ---- delete ---------------------------------------------------------------

@snapshot_group.command("delete")
@click.argument("snapshot_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def snapshot_delete(obj, snapshot_id: str, output_format: str | None) -> None:
    """Delete a snapshot by ID."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    mgr = obj.get_manager()
    with obj.output.spinner(f"Deleting snapshot {snapshot_id}..."):
        mgr.delete_snapshot(snapshot_id)
    obj.output.success(f"Snapshot deleted: {snapshot_id}")
