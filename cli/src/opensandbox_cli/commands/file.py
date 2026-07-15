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

"""File operation commands: cat, write, upload, download, rm, mv, mkdir, rmdir, search, info, chmod, replace."""

from __future__ import annotations

import sys
from pathlib import Path

import click

from opensandbox_cli.client import ClientContext
from opensandbox_cli.utils import handle_errors, output_option, prepare_output


def _parse_permission_mode(mode: str) -> int:
    """Parse a permission mode string like 644 or 755."""
    try:
        return int(mode)
    except ValueError as exc:
        raise click.BadParameter(
            f"Invalid permission mode '{mode}'. Use a permission value like 644 or 755."
        ) from exc


@click.group("file", invoke_without_command=True)
@click.pass_context
def file_group(ctx: click.Context) -> None:
    """📁 File operations on a sandbox."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


# ---- cat (read) -----------------------------------------------------------

@file_group.command("cat")
@click.argument("sandbox_id")
@click.argument("path")
@click.option("--encoding", default="utf-8", help="File encoding.")
@output_option("raw", help_text="Output format: raw.")
@click.pass_obj
@handle_errors
def file_cat(
    obj: ClientContext,
    sandbox_id: str,
    path: str,
    encoding: str,
    output_format: str | None,
) -> None:
    """Read a file from the sandbox."""
    prepare_output(obj, output_format, allowed=("raw",), fallback="raw")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        content = sandbox.files.read_file(path, encoding=encoding)
        click.echo(content, nl=False)
    finally:
        sandbox.close()


# ---- write ----------------------------------------------------------------

@file_group.command("write")
@click.argument("sandbox_id")
@click.argument("path")
@click.option("--content", "-c", default=None, help="Content to write. Reads from stdin if not provided.")
@click.option("--encoding", default="utf-8", help="File encoding.")
@click.option("--mode", default=None, help="File permission mode (e.g. 0644).")
@click.option("--owner", default=None, help="File owner.")
@click.option("--group", default=None, help="File group.")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def file_write(
    obj: ClientContext,
    sandbox_id: str,
    path: str,
    content: str | None,
    encoding: str,
    mode: str | None,
    owner: str | None,
    group: str | None,
    output_format: str | None,
) -> None:
    """Write content to a file in the sandbox."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    file_content = content
    if file_content is None:
        if sys.stdin.isatty():
            click.echo("Reading from stdin (Ctrl+D to finish):", err=True)
        file_content = sys.stdin.read()

    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        kwargs: dict = {"encoding": encoding}
        if mode is not None:
            kwargs["mode"] = _parse_permission_mode(mode)
        if owner is not None:
            kwargs["owner"] = owner
        if group is not None:
            kwargs["group"] = group
        sandbox.files.write_file(path, file_content, **kwargs)
        obj.output.success(f"Written: {path}")
    finally:
        sandbox.close()


# ---- upload ---------------------------------------------------------------

@file_group.command("upload")
@click.argument("sandbox_id")
@click.argument("local_path", type=click.Path(exists=True))
@click.argument("remote_path")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def file_upload(
    obj: ClientContext,
    sandbox_id: str,
    local_path: str,
    remote_path: str,
    output_format: str | None,
) -> None:
    """Upload a local file to the sandbox."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        with Path(local_path).open("rb") as data:
            sandbox.files.write_file(remote_path, data)
        obj.output.success(f"Uploaded: {local_path} → {remote_path}")
    finally:
        sandbox.close()


# ---- download -------------------------------------------------------------

@file_group.command("download")
@click.argument("sandbox_id")
@click.argument("remote_path")
@click.argument("local_path", type=click.Path())
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def file_download(
    obj: ClientContext,
    sandbox_id: str,
    remote_path: str,
    local_path: str,
    output_format: str | None,
) -> None:
    """Download a file from the sandbox to local disk."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        destination = Path(local_path)
        destination.parent.mkdir(parents=True, exist_ok=True)
        with destination.open("wb") as out:
            for chunk in sandbox.files.read_bytes_stream(remote_path):
                out.write(chunk)
        obj.output.success(f"Downloaded: {remote_path} → {local_path}")
    finally:
        sandbox.close()


# ---- rm (delete) ----------------------------------------------------------

@file_group.command("rm")
@click.argument("sandbox_id")
@click.argument("paths", nargs=-1, required=True)
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def file_rm(
    obj: ClientContext,
    sandbox_id: str,
    paths: tuple[str, ...],
    output_format: str | None,
) -> None:
    """Delete files from the sandbox."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        sandbox.files.delete_files(list(paths))
        obj.output.print_rows(
            [{"path": p, "status": "deleted"} for p in paths],
            columns=["path", "status"],
            title="Deleted Files",
        )
    finally:
        sandbox.close()


# ---- mv (move) ------------------------------------------------------------

@file_group.command("mv")
@click.argument("sandbox_id")
@click.argument("source")
@click.argument("destination")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def file_mv(
    obj: ClientContext,
    sandbox_id: str,
    source: str,
    destination: str,
    output_format: str | None,
) -> None:
    """Move/rename a file in the sandbox."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    from opensandbox.models.filesystem import MoveEntry

    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        sandbox.files.move_files([MoveEntry(source=source, destination=destination)])
        obj.output.success(f"Moved: {source} → {destination}")
    finally:
        sandbox.close()


# ---- mkdir ----------------------------------------------------------------

@file_group.command("mkdir")
@click.argument("sandbox_id")
@click.argument("paths", nargs=-1, required=True)
@click.option("--mode", default=None, help="Directory permission mode.")
@click.option("--owner", default=None, help="Directory owner.")
@click.option("--group", default=None, help="Directory group.")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def file_mkdir(
    obj: ClientContext,
    sandbox_id: str,
    paths: tuple[str, ...],
    mode: str | None,
    owner: str | None,
    group: str | None,
    output_format: str | None,
) -> None:
    """Create directories in the sandbox."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    from opensandbox.models.filesystem import WriteEntry

    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        entries = []
        for p in paths:
            kwargs: dict = {"path": p}
            if mode is not None:
                kwargs["mode"] = _parse_permission_mode(mode)
            if owner is not None:
                kwargs["owner"] = owner
            if group is not None:
                kwargs["group"] = group
            entries.append(WriteEntry(**kwargs))
        sandbox.files.create_directories(entries)
        obj.output.print_rows(
            [{"path": p, "status": "created"} for p in paths],
            columns=["path", "status"],
            title="Created Directories",
        )
    finally:
        sandbox.close()


# ---- rmdir ----------------------------------------------------------------

@file_group.command("rmdir")
@click.argument("sandbox_id")
@click.argument("paths", nargs=-1, required=True)
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def file_rmdir(
    obj: ClientContext,
    sandbox_id: str,
    paths: tuple[str, ...],
    output_format: str | None,
) -> None:
    """Delete directories from the sandbox."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        sandbox.files.delete_directories(list(paths))
        obj.output.print_rows(
            [{"path": p, "status": "removed"} for p in paths],
            columns=["path", "status"],
            title="Removed Directories",
        )
    finally:
        sandbox.close()


# ---- search ---------------------------------------------------------------

@file_group.command("search")
@click.argument("sandbox_id")
@click.argument("path")
@click.option("--pattern", "-p", required=True, help="Glob pattern to search for.")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def file_search(
    obj: ClientContext,
    sandbox_id: str,
    path: str,
    pattern: str,
    output_format: str | None,
) -> None:
    """Search for files in the sandbox."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    from opensandbox.models.filesystem import SearchEntry

    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        results = sandbox.files.search(SearchEntry(path=path, pattern=pattern))
        if not results:
            if obj.output.fmt in ("json", "yaml"):
                obj.output.print_models([], columns=[])
            else:
                obj.output.info("No files found.")
            return
        if obj.output.fmt in ("json", "yaml"):
            obj.output.print_models(results, columns=["path", "size", "mode", "owner", "modified_at"])
        else:
            obj.output.print_models(results, columns=["path", "size", "owner"], title="Search Results")
    finally:
        sandbox.close()


# ---- info (stat) ----------------------------------------------------------

@file_group.command("info")
@click.argument("sandbox_id")
@click.argument("paths", nargs=-1, required=True)
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def file_info(
    obj: ClientContext,
    sandbox_id: str,
    paths: tuple[str, ...],
    output_format: str | None,
) -> None:
    """Get file/directory info."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        info_map = sandbox.files.get_file_info(list(paths))
        rows = [{"path": path, **entry.model_dump(mode="json")} for path, entry in info_map.items()]
        obj.output.print_rows(
            rows,
            columns=["path", "size", "mode", "owner", "group", "created_at", "modified_at"],
            title="Path Info",
        )
    finally:
        sandbox.close()


# ---- chmod ----------------------------------------------------------------

@file_group.command("chmod")
@click.argument("sandbox_id")
@click.argument("path")
@click.option("--mode", required=True, help="Permission mode (e.g. 0755).")
@click.option("--owner", default=None, help="File owner.")
@click.option("--group", default=None, help="File group.")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def file_chmod(
    obj: ClientContext,
    sandbox_id: str,
    path: str,
    mode: str,
    owner: str | None,
    group: str | None,
    output_format: str | None,
) -> None:
    """Set file permissions."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    from opensandbox.models.filesystem import SetPermissionEntry

    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        sandbox.files.set_permissions(
            [
                SetPermissionEntry(
                    path=path,
                    mode=_parse_permission_mode(mode),
                    owner=owner,
                    group=group,
                )
            ]
        )
        obj.output.success(f"Permissions set: {path}")
    finally:
        sandbox.close()


# ---- replace --------------------------------------------------------------

@file_group.command("replace")
@click.argument("sandbox_id")
@click.argument("path")
@click.option("--old", required=True, help="Text to search for.")
@click.option("--new", required=True, help="Replacement text.")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def file_replace(
    obj: ClientContext,
    sandbox_id: str,
    path: str,
    old: str,
    new: str,
    output_format: str | None,
) -> None:
    """Replace content in a file."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    from opensandbox.models.filesystem import ContentReplaceEntry

    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        sandbox.files.replace_contents(
            [ContentReplaceEntry(path=path, old_content=old, new_content=new)]
        )
        obj.output.success(f"Replaced in: {path}")
    finally:
        sandbox.close()
