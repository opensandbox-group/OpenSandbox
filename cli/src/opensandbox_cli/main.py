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

"""Root Click group with global options."""

from __future__ import annotations

from pathlib import Path

import click
from rich.console import Console

from opensandbox_cli import __version__
from opensandbox_cli.client import ClientContext
from opensandbox_cli.commands.command import command_group
from opensandbox_cli.commands.config_cmd import config_group
from opensandbox_cli.commands.devops import devops_group
from opensandbox_cli.commands.diagnostics import diagnostics_group
from opensandbox_cli.commands.egress import egress_group
from opensandbox_cli.commands.file import file_group
from opensandbox_cli.commands.sandbox import sandbox_group
from opensandbox_cli.commands.skills import skills_group
from opensandbox_cli.config import resolve_config

# ---------------------------------------------------------------------------
# Banner
# ---------------------------------------------------------------------------

BANNER = r"""[bold cyan]
   ____                   _____                 _ _
  / __ \                 / ____|               | | |
 | |  | |_ __   ___ _ _| (___   __ _ _ __   __| | |__   _____  __
 | |  | | '_ \ / _ \ '_ \___ \ / _` | '_ \ / _` | '_ \ / _ \ \/ /
 | |__| | |_) |  __/ | | |___) | (_| | | | | (_| | |_) | (_) >  <
  \____/| .__/ \___|_| |_|____/ \__,_|_| |_|\__,_|_.__/ \___/_/\_\
        | |
        |_|[/]  [dim]v{version}[/]
"""


class BannerGroup(click.Group):
    """Custom Click group that shows a banner before help text."""

    def format_help(self, ctx: click.Context, formatter: click.HelpFormatter) -> None:
        console = Console(stderr=False)
        console.print(BANNER.format(version=__version__))
        super().format_help(ctx, formatter)


@click.group(cls=BannerGroup, context_settings={"help_option_names": ["-h", "--help"]})
@click.option("--api-key", envvar="OPEN_SANDBOX_API_KEY", default=None, help="API key for authentication.")
@click.option("--domain", envvar="OPEN_SANDBOX_DOMAIN", default=None, help="API server domain (e.g. localhost:8080).")
@click.option("--protocol", type=click.Choice(["http", "https"]), default=None, help="Protocol (http/https).")
@click.option("--request-timeout", type=int, default=None, help="Request timeout in seconds.")
@click.option(
    "--use-server-proxy/--no-use-server-proxy",
    default=None,
    help="Route execd and endpoint traffic through the sandbox server proxy.",
)
@click.option("--config", "config_path", type=click.Path(exists=False, path_type=Path), default=None, help="Config file path.")
@click.option("-v", "--verbose", is_flag=True, default=False, help="Enable verbose/debug output.")
@click.option("--no-color", is_flag=True, default=False, help="Disable colored output.")
@click.version_option(version=__version__, prog_name="opensandbox")
@click.pass_context
def cli(
    ctx: click.Context,
    api_key: str | None,
    domain: str | None,
    protocol: str | None,
    request_timeout: int | None,
    use_server_proxy: bool | None,
    config_path: Path | None,
    verbose: bool,
    no_color: bool,
) -> None:
    """OpenSandbox CLI — manage sandboxes from your terminal."""
    if verbose:
        import logging

        logging.basicConfig(level=logging.DEBUG)

    cli_overrides = {
        "api_key": api_key,
        "domain": domain,
        "protocol": protocol,
        "request_timeout": request_timeout,
        "use_server_proxy": use_server_proxy,
    }

    if ctx.invoked_subcommand == "config":
        resolved = {
            "api_key": api_key,
            "domain": domain,
            "protocol": protocol or "http",
            "request_timeout": request_timeout or 30,
            "use_server_proxy": use_server_proxy if use_server_proxy is not None else False,
            "color": True,
            "default_image": None,
            "default_timeout": None,
        }
    else:
        resolved = resolve_config(
            cli_api_key=api_key,
            cli_domain=domain,
            cli_protocol=protocol,
            cli_timeout=request_timeout,
            cli_use_server_proxy=use_server_proxy,
            config_path=config_path,
        )
    resolved["color"] = not no_color and resolved.get("color", True)

    effective_config_path = config_path or Path.home() / ".opensandbox" / "config.toml"
    ctx.obj = ClientContext(
        resolved_config=resolved,
        config_path=effective_config_path,
        cli_overrides=cli_overrides,
    )
    ctx.call_on_close(lambda: ctx.obj.close())


# Register sub-command groups
cli.add_command(sandbox_group)
cli.add_command(command_group)
cli.add_command(file_group)
cli.add_command(egress_group)
cli.add_command(config_group)
cli.add_command(diagnostics_group)
cli.add_command(devops_group)
cli.add_command(skills_group)
