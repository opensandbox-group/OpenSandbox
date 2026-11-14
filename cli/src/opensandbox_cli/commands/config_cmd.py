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
from opensandbox_cli.config import CONFIG_VALUE_KINDS, init_config_file, resolve_config
from opensandbox_cli.utils import (
    handle_errors,
    output_option,
    parse_nullable_duration,
    prepare_output,
)


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
        output.error(str(exc))
        raise click.exceptions.Exit(1) from None


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


def _validated_config_value(key: str, raw: str) -> Any:
    """Validate a ``config set`` value against the key's declared shape.

    Returns the TOML value to store. Raises ``click.ClickException`` for an
    invalid value so the mistake is caught at ``config set`` time instead of
    surfacing as a validation error on every later command.
    """
    kind = CONFIG_VALUE_KINDS[key]
    if kind == "verbatim":
        return raw
    if kind == "protocol":
        normalized = raw.strip().lower()
        if normalized not in ("http", "https"):
            raise click.ClickException(
                f"Invalid value for {key}: '{raw}'. Protocol must be 'http' or 'https'."
            )
        return normalized
    if kind == "int":
        # Positive-only: the keys using this kind feed the SDK's
        # positive-timeout validation, and 0 would be silently dropped by
        # resolve_config's falsy fallback chain.
        parsed_int: int | None = None
        try:
            parsed_int = int(raw)
        except ValueError:
            try:
                as_float = float(raw)
            except ValueError:
                as_float = None
            if as_float is not None and as_float.is_integer():
                parsed_int = int(as_float)
        if parsed_int is None or parsed_int <= 0:
            raise click.ClickException(
                f"Invalid value for {key}: '{raw}'. Must be a positive whole number."
            ) from None
        return parsed_int
    if kind == "bool":
        normalized = raw.strip().lower()
        if normalized in ("1", "true", "yes", "on"):
            return True
        if normalized in ("0", "false", "no", "off"):
            return False
        as_number: float | None = None
        try:
            as_number = float(raw)
        except ValueError:
            as_number = None
        if as_number is not None and as_number in (0, 1):
            return bool(as_number)
        raise click.ClickException(
            f"Invalid value for {key}: '{raw}'. Must be a boolean (true/false)."
        )
    if kind != "duration-or-none":
        raise click.ClickException(f"Unsupported value kind '{kind}' for {key}.")
    try:
        parse_nullable_duration(raw)
    except click.BadParameter as exc:
        raise click.ClickException(
            f"Invalid value for {key}: '{raw}'. Use format like 10m, 1h30m, 90s, or 'none'."
        ) from exc
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
    if key not in CONFIG_VALUE_KINDS:
        known_keys = ", ".join(sorted(CONFIG_VALUE_KINDS))
        raise click.ClickException(
            f"Unknown config key '{key}'. Known keys: {known_keys}."
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

    table[field] = _validated_config_value(key, value)
    path.write_text(tomlkit.dumps(document), encoding="utf-8")

    obj.output.success(f"Set {key} = {value}")
