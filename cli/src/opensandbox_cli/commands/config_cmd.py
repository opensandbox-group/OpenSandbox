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

"""Config management commands: init, show, set."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

import click
import tomlkit
from tomlkit.container import OutOfOrderTableProxy
from tomlkit.exceptions import ParseError
from tomlkit.items import InlineTable, Table

from opensandbox_cli.client import ClientContext
from opensandbox_cli.config import init_config_file, resolve_config
from opensandbox_cli.utils import handle_errors, output_option, prepare_output


@click.group("config", invoke_without_command=True)
@click.pass_context
def config_group(ctx: click.Context) -> None:
    """⚙️  Manage CLI configuration."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


@config_group.command("init")
@click.option("--force", is_flag=True, default=False, help="Overwrite existing config file.")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def config_init(obj: ClientContext, force: bool, output_format: str | None) -> None:
    """Create a default configuration file."""
    output = prepare_output(
        obj, output_format, allowed=("table", "json", "yaml"), fallback="table"
    )

    try:
        path = init_config_file(obj.config_path, force=force)
        output.success(f"Config file created: {path}")
    except FileExistsError as exc:
        output.warning(str(exc))


_SENSITIVE_CONFIG_KEYS = {
    "api_key",
}


def _mask_secret(value: str | None) -> str | None:
    """Mask a secret while keeping enough shape for debugging."""
    if value is None:
        return None
    if len(value) <= 4:
        return "*" * len(value)
    return f"{value[:2]}{'*' * (len(value) - 4)}{value[-2:]}"


def _sanitize_config_for_display(data: Mapping[str, Any]) -> dict[str, Any]:
    """Return a display-safe copy of the resolved config."""
    sanitized: dict[str, Any] = {}
    for key, value in data.items():
        if key in _SENSITIVE_CONFIG_KEYS:
            sanitized[key] = _mask_secret(value if isinstance(value, str) else None)
            continue
        sanitized[key] = value
    return sanitized


@config_group.command("show")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def config_show(obj: ClientContext, output_format: str | None) -> None:
    """Show the resolved configuration."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    resolved = resolve_config(
        cli_api_key=obj.cli_overrides.get("api_key"),
        cli_domain=obj.cli_overrides.get("domain"),
        cli_protocol=obj.cli_overrides.get("protocol"),
        cli_timeout=obj.cli_overrides.get("request_timeout"),
        cli_use_server_proxy=obj.cli_overrides.get("use_server_proxy"),
        config_path=obj.config_path,
    )
    obj.output.print_dict(
        {
            **_sanitize_config_for_display(resolved),
            "config_path": str(obj.config_path),
            "config_file_exists": obj.config_path.exists(),
        },
        title="Resolved Configuration",
    )


def _parse_toml_value(raw: str) -> Any:
    """Infer a TOML scalar type from a raw CLI string.

    Booleans, integers, and floats are recognized so values like ``false`` or
    ``30`` keep their native TOML type; anything else is stored as a string.
    The value is always serialized by tomlkit, so quotes, escapes, and
    whitespace in string values are encoded correctly.
    """
    normalized = raw.lower()
    if normalized in ("true", "false"):
        return normalized == "true"
    try:
        return int(raw)
    except ValueError:
        pass
    try:
        return float(raw)
    except ValueError:
        pass
    return raw


@config_group.command("set")
@click.argument("key")
@click.argument("value")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def config_set(
    obj: ClientContext,
    key: str,
    value: str,
    output_format: str | None,
) -> None:
    """Set a configuration value (e.g. 'connection.domain' 'localhost:9090')."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    path = obj.config_path
    if not path.exists():
        raise click.ClickException(f"Config file not found: {path}. Run 'osb config init' first.")

    section, _, field = key.partition(".")
    if not section or not field or "." in field:
        raise click.ClickException(
            "Key must be in 'section.field' format (e.g. connection.domain)."
        )

    try:
        document = tomlkit.parse(path.read_text(encoding="utf-8"))
    except ParseError as exc:
        raise click.ClickException(
            f"Config file is not valid TOML ({path}): {exc}"
        ) from exc

    table = document.get(section)
    if table is None:
        table = tomlkit.table()
        document[section] = table
    if not isinstance(table, (Table, InlineTable, OutOfOrderTableProxy)):
        raise click.ClickException(f"Cannot set {key}: '{section}' is not a TOML table.")

    table[field] = _parse_toml_value(value)
    path.write_text(tomlkit.dumps(document), encoding="utf-8")

    obj.output.success(f"Set {key} = {value}")
