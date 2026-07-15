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

"""Config management commands: init, show, set."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

import click

from opensandbox_cli.client import ClientContext
from opensandbox_cli.config import init_config_file, resolve_config
from opensandbox_cli.utils import handle_errors, output_option, prepare_output


@click.group("config", invoke_without_command=True)
@click.pass_context
def config_group(ctx: click.Context) -> None:
    """⚙️  Manage CLI configuration."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


# ---- init -----------------------------------------------------------------

@config_group.command("init")
@click.option("--force", is_flag=True, default=False, help="Overwrite existing config file.")
@output_option("table", "json", "yaml")
@handle_errors
@click.pass_obj
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


# ---- show -----------------------------------------------------------------

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


# ---- set ------------------------------------------------------------------

@config_group.command("set")
@click.argument("key")
@click.argument("value")
@output_option("table", "json", "yaml")
@handle_errors
@click.pass_obj
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

    content = path.read_text()

    # TODO: Replace this regex-based TOML editing with a parser-backed update
    # path so formatting/comments survive reliably as config complexity grows.
    # Simple key replacement in TOML
    # Supports dotted keys like connection.domain
    parts = key.split(".", 1)
    if len(parts) == 2:
        section, field = parts
        # Try to find and update existing value
        import re

        section_pattern = rf"(\[{re.escape(section)}\].*?)(?=\n\[|\Z)"
        section_match = re.search(section_pattern, content, re.DOTALL)

        # Infer TOML value type: bool > int > float > string
        def _toml_value(raw: str) -> str:
            if raw.lower() in ("true", "false"):
                return raw.lower()
            try:
                int(raw)
                return raw
            except ValueError:
                pass
            try:
                float(raw)
                return raw
            except ValueError:
                pass
            return f'"{raw}"'

        toml_val = _toml_value(value)

        if section_match:
            section_text = section_match.group(1)
            field_pattern = rf'^(#?\s*{re.escape(field)}\s*=\s*).*$'
            field_match = re.search(field_pattern, section_text, re.MULTILINE)
            if field_match:
                new_line = f'{field} = {toml_val}'
                new_section = section_text[:field_match.start()] + new_line + section_text[field_match.end():]
                content = content[:section_match.start()] + new_section + content[section_match.end():]
            else:
                # Add field to section
                insert_pos = section_match.end()
                content = content[:insert_pos] + f'\n{field} = {toml_val}' + content[insert_pos:]
        else:
            # Add new section
            content += f'\n[{section}]\n{field} = {toml_val}\n'
    else:
        raise click.ClickException(
            "Key must be in 'section.field' format (e.g. connection.domain)."
        )

    path.write_text(content)

    obj.output.success(f"Set {key} = {value}")
