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

"""Runtime egress policy commands."""

from __future__ import annotations

import click
from opensandbox.models.sandboxes import NetworkRule

from opensandbox_cli.client import ClientContext
from opensandbox_cli.utils import handle_errors, output_option, prepare_output


def _parse_rule(value: str) -> NetworkRule:
    """Parse ACTION=TARGET into a NetworkRule."""
    action, sep, target = value.partition("=")
    if not sep:
        raise click.BadParameter(
            f"Invalid rule '{value}'. Use ACTION=TARGET, for example allow=pypi.org."
        )
    action = action.strip().lower()
    target = target.strip()
    if action not in ("allow", "deny"):
        raise click.BadParameter(
            f"Invalid rule action '{action}'. Use allow or deny."
        )
    return NetworkRule(action=action, target=target)


@click.group("egress", invoke_without_command=True)
@click.pass_context
def egress_group(ctx: click.Context) -> None:
    """Manage runtime egress policy for a sandbox."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


@egress_group.command("get")
@click.argument("sandbox_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def egress_get(obj: ClientContext, sandbox_id: str, output_format: str | None) -> None:
    """Get the current egress policy."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        policy = sandbox.get_egress_policy()
        obj.output.print_model(policy, title="Egress Policy")
    finally:
        sandbox.close()


@egress_group.command("patch")
@click.argument("sandbox_id")
@click.option(
    "--rule",
    "rules",
    multiple=True,
    required=True,
    help="Patch rule in ACTION=TARGET form. Repeatable, e.g. --rule allow=pypi.org.",
)
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def egress_patch(
    obj: ClientContext,
    sandbox_id: str,
    rules: tuple[str, ...],
    output_format: str | None,
) -> None:
    """Patch runtime egress rules with merge semantics."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    parsed_rules = [_parse_rule(rule) for rule in rules]
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        sandbox.patch_egress_rules(parsed_rules)
        obj.output.success_panel(
            {
                "sandbox_id": sandbox.id,
                "patched_rules": [rule.model_dump(mode="json") for rule in parsed_rules],
            },
            title="Egress Policy Patched",
        )
    finally:
        sandbox.close()
