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

"""Sandbox-local Credential Vault commands."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import click
import yaml

from opensandbox_cli.client import ClientContext
from opensandbox_cli.utils import handle_errors, output_option, prepare_output


def _read_payload(payload_file: str) -> dict[str, Any]:
    """Read a JSON/YAML object from a file path or stdin."""
    try:
        if payload_file == "-":
            raw = click.get_text_stream("stdin").read()
        else:
            path = Path(payload_file)
            if not path.is_file():
                raise click.ClickException(f"Payload file not found: {payload_file}")
            raw = path.read_text(encoding="utf-8")
        payload = yaml.safe_load(raw)
    except click.ClickException:
        raise
    except Exception as exc:
        raise click.ClickException(
            f"Failed to read credential vault payload from {payload_file}."
        ) from exc

    if not isinstance(payload, dict):
        raise click.ClickException("Credential vault payload must be a JSON/YAML object.")
    return payload


def _require_list(payload: dict[str, Any], key: str) -> list[Any]:
    value = payload.get(key)
    if not isinstance(value, list):
        raise click.ClickException(f"Credential vault payload field '{key}' must be a list.")
    return value


def _get_expected_revision(payload: dict[str, Any]) -> int | None:
    raw = payload.get("expectedRevision", payload.get("expected_revision"))
    if raw is None:
        return None
    if type(raw) is not int:
        raise click.ClickException("Credential vault expectedRevision must be an integer.")
    return raw


def _optional_object(payload: dict[str, Any], key: str) -> dict[str, Any] | None:
    value = payload.get(key)
    if value is None:
        return None
    if not isinstance(value, dict):
        raise click.ClickException(f"Credential vault patch field '{key}' must be an object.")
    return value


@click.group("credential-vault", invoke_without_command=True)
@click.pass_context
def credential_vault_group(ctx: click.Context) -> None:
    """Manage sandbox-local Credential Vault state."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


@credential_vault_group.command("create")
@click.argument("sandbox_id")
@click.option(
    "--file",
    "payload_file",
    required=True,
    help="JSON/YAML payload file, or '-' to read from stdin.",
)
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def credential_vault_create(
    obj: ClientContext,
    sandbox_id: str,
    payload_file: str,
    output_format: str | None,
) -> None:
    """Create the initial Credential Vault state."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    payload = _read_payload(payload_file)
    credentials = _require_list(payload, "credentials")
    bindings = _require_list(payload, "bindings")

    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        state = sandbox.credential_vault.create(
            credentials=credentials,
            bindings=bindings,
        )
        obj.output.print_model(state, title="Credential Vault")
    finally:
        sandbox.close()


@credential_vault_group.command("get")
@click.argument("sandbox_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def credential_vault_get(
    obj: ClientContext,
    sandbox_id: str,
    output_format: str | None,
) -> None:
    """Get sanitized Credential Vault state."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        state = sandbox.credential_vault.get()
        obj.output.print_model(state, title="Credential Vault")
    finally:
        sandbox.close()


@credential_vault_group.command("patch")
@click.argument("sandbox_id")
@click.option(
    "--file",
    "payload_file",
    required=True,
    help="JSON/YAML mutation payload file, or '-' to read from stdin.",
)
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def credential_vault_patch(
    obj: ClientContext,
    sandbox_id: str,
    payload_file: str,
    output_format: str | None,
) -> None:
    """Atomically mutate credentials and bindings."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    payload = _read_payload(payload_file)
    credentials = _optional_object(payload, "credentials")
    bindings = _optional_object(payload, "bindings")
    if credentials is None and bindings is None:
        raise click.ClickException(
            "Credential vault patch payload must include credentials or bindings."
        )

    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        state = sandbox.credential_vault.patch(
            expected_revision=_get_expected_revision(payload),
            credentials=credentials,
            bindings=bindings,
        )
        obj.output.print_model(state, title="Credential Vault")
    finally:
        sandbox.close()


@credential_vault_group.command("delete")
@click.argument("sandbox_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def credential_vault_delete(
    obj: ClientContext,
    sandbox_id: str,
    output_format: str | None,
) -> None:
    """Delete the sandbox-local Credential Vault."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        sandbox.credential_vault.delete()
        obj.output.success_panel(
            {"sandbox_id": sandbox.id, "status": "deleted"},
            title="Credential Vault Deleted",
        )
    finally:
        sandbox.close()


@credential_vault_group.group("credential", invoke_without_command=True)
@click.pass_context
def credential_group(ctx: click.Context) -> None:
    """Inspect sanitized Credential Vault credential metadata."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


@credential_group.command("list")
@click.argument("sandbox_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def credential_list(
    obj: ClientContext,
    sandbox_id: str,
    output_format: str | None,
) -> None:
    """List sanitized credential metadata."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        credentials = sandbox.credential_vault.list_credentials()
        obj.output.print_models(
            credentials,
            columns=["name", "source_type", "revision"],
            title="Credential Vault Credentials",
        )
    finally:
        sandbox.close()


@credential_group.command("get")
@click.argument("sandbox_id")
@click.argument("credential_name")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def credential_get(
    obj: ClientContext,
    sandbox_id: str,
    credential_name: str,
    output_format: str | None,
) -> None:
    """Get sanitized metadata for one credential."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        credential = sandbox.credential_vault.get_credential(credential_name)
        obj.output.print_model(credential, title="Credential Vault Credential")
    finally:
        sandbox.close()


@credential_vault_group.group("binding", invoke_without_command=True)
@click.pass_context
def binding_group(ctx: click.Context) -> None:
    """Inspect sanitized Credential Vault binding metadata."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


@binding_group.command("list")
@click.argument("sandbox_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def binding_list(
    obj: ClientContext,
    sandbox_id: str,
    output_format: str | None,
) -> None:
    """List sanitized binding metadata."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        bindings = sandbox.credential_vault.list_bindings()
        obj.output.print_models(
            bindings,
            columns=["name", "revision", "match", "auth"],
            title="Credential Vault Bindings",
        )
    finally:
        sandbox.close()


@binding_group.command("get")
@click.argument("sandbox_id")
@click.argument("binding_name")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def binding_get(
    obj: ClientContext,
    sandbox_id: str,
    binding_name: str,
    output_format: str | None,
) -> None:
    """Get sanitized metadata for one binding."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        binding = sandbox.credential_vault.get_binding(binding_name)
        obj.output.print_model(binding, title="Credential Vault Binding")
    finally:
        sandbox.close()
