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

from __future__ import annotations

from collections.abc import Iterator

import click
import pytest
from click.testing import CliRunner

from opensandbox_cli import plugins
from opensandbox_cli.client import ClientContext
from opensandbox_cli.commands.sandbox import sandbox_group
from opensandbox_cli.main import cli
from opensandbox_cli.plugins import load_plugin_commands


class FakeEntryPoint:
    """Minimal stand-in for ``importlib.metadata.EntryPoint``."""

    def __init__(
        self,
        name: str,
        target: object | None = None,
        error: BaseException | None = None,
    ) -> None:
        self.name = name
        self._target = target
        self._error = error
        self.loaded = False

    def load(self) -> object:
        self.loaded = True
        if self._error is not None:
            raise self._error
        return self._target


@click.group()
def workflow() -> None:
    """Run team workflows against OpenSandbox."""


@workflow.command()
def run() -> None:
    """Run a named workflow."""


@pytest.fixture()
def isolated_cli() -> Iterator[click.Group]:
    """Restore the root group's commands after a test registers a plugin."""
    original = dict(cli.commands)
    try:
        yield cli
    finally:
        cli.commands.clear()
        cli.commands.update(original)


@pytest.fixture()
def runner() -> CliRunner:
    return CliRunner()


class TestPluginLoading:
    def test_group_is_registered_and_listed(
        self, runner: CliRunner, isolated_cli: click.Group
    ) -> None:
        registered = load_plugin_commands(
            isolated_cli, entry_points=[FakeEntryPoint("workflow", workflow)]
        )

        assert registered == ["workflow"]
        root_help = runner.invoke(isolated_cli, ["--help"])
        assert root_help.exit_code == 0
        assert "workflow" in root_help.output
        plugin_help = runner.invoke(isolated_cli, ["workflow", "--help"])
        assert plugin_help.exit_code == 0
        assert "run" in plugin_help.output
        plugin_command_help = runner.invoke(isolated_cli, ["workflow", "run", "--help"])
        assert plugin_command_help.exit_code == 0
        assert "Run a named workflow" in plugin_command_help.output

    def test_command_is_registered_under_entry_point_name(
        self, runner: CliRunner, isolated_cli: click.Group
    ) -> None:
        @click.command()
        def hello() -> None:
            click.echo("hello from plugin")

        registered = load_plugin_commands(
            isolated_cli, entry_points=[FakeEntryPoint("greet", hello)]
        )

        assert registered == ["greet"]
        result = runner.invoke(isolated_cli, ["greet"])
        assert result.exit_code == 0
        assert "hello from plugin" in result.output

    def test_plugin_command_receives_root_context(
        self, runner: CliRunner, isolated_cli: click.Group
    ) -> None:
        @click.command()
        @click.pass_context
        def check_context(ctx: click.Context) -> None:
            assert isinstance(ctx.obj, ClientContext)
            click.echo("context ok")

        load_plugin_commands(
            isolated_cli, entry_points=[FakeEntryPoint("check-context", check_context)]
        )

        result = runner.invoke(isolated_cli, ["check-context"])

        assert result.exit_code == 0
        assert "context ok" in result.output

    def test_builtin_name_collision_is_skipped(
        self,
        runner: CliRunner,
        isolated_cli: click.Group,
        capsys: pytest.CaptureFixture[str],
    ) -> None:
        entry_point = FakeEntryPoint("sandbox", click.Command("sandbox"))

        registered = load_plugin_commands(isolated_cli, entry_points=[entry_point])

        assert registered == []
        assert entry_point.loaded is False
        assert isolated_cli.commands["sandbox"] is sandbox_group
        warning = capsys.readouterr().err
        assert "Warning:" in warning
        assert "sandbox" in warning
        assert "already registered" in warning
        assert warning.count("\n") == 1
        assert runner.invoke(isolated_cli, ["sandbox", "--help"]).exit_code == 0

    def test_load_error_is_skipped(
        self,
        runner: CliRunner,
        isolated_cli: click.Group,
        capsys: pytest.CaptureFixture[str],
    ) -> None:
        entry_point = FakeEntryPoint("broken", error=RuntimeError("boom"))
        original_commands = dict(isolated_cli.commands)

        registered = load_plugin_commands(isolated_cli, entry_points=[entry_point])

        assert registered == []
        assert entry_point.loaded is True
        assert isolated_cli.commands == original_commands
        warning = capsys.readouterr().err
        assert "Warning:" in warning
        assert "broken" in warning
        assert "failed to load" in warning
        assert "boom" in warning
        assert warning.count("\n") == 1
        root_help = runner.invoke(isolated_cli, ["--help"])
        assert root_help.exit_code == 0
        assert "sandbox" in root_help.output

    def test_system_exit_from_plugin_is_skipped(
        self,
        runner: CliRunner,
        isolated_cli: click.Group,
        capsys: pytest.CaptureFixture[str],
    ) -> None:
        entry_point = FakeEntryPoint("exiting", error=SystemExit(2))
        original_commands = dict(isolated_cli.commands)

        registered = load_plugin_commands(isolated_cli, entry_points=[entry_point])

        assert registered == []
        assert entry_point.loaded is True
        assert isolated_cli.commands == original_commands
        warning = capsys.readouterr().err
        assert "Warning:" in warning
        assert "exiting" in warning
        assert "failed to load" in warning
        assert "SystemExit" in warning
        assert warning.count("\n") == 1
        assert runner.invoke(isolated_cli, ["--help"]).exit_code == 0

    def test_keyboard_interrupt_from_plugin_propagates(
        self, isolated_cli: click.Group
    ) -> None:
        entry_point = FakeEntryPoint("interrupted", error=KeyboardInterrupt())

        with pytest.raises(KeyboardInterrupt):
            load_plugin_commands(isolated_cli, entry_points=[entry_point])

    def test_discovery_error_is_skipped(
        self,
        runner: CliRunner,
        isolated_cli: click.Group,
        monkeypatch: pytest.MonkeyPatch,
        capsys: pytest.CaptureFixture[str],
    ) -> None:
        def broken_discovery() -> list[FakeEntryPoint]:
            raise RuntimeError("malformed entry_points.txt")

        monkeypatch.setattr(plugins, "_installed_entry_points", broken_discovery)
        original_commands = dict(isolated_cli.commands)

        registered = load_plugin_commands(isolated_cli)

        assert registered == []
        assert isolated_cli.commands == original_commands
        warning = capsys.readouterr().err
        assert "Warning:" in warning
        assert "failed to read entry points" in warning
        assert "malformed entry_points.txt" in warning
        assert warning.count("\n") == 1
        assert runner.invoke(isolated_cli, ["--help"]).exit_code == 0

    def test_non_click_object_is_skipped(
        self,
        runner: CliRunner,
        isolated_cli: click.Group,
        capsys: pytest.CaptureFixture[str],
    ) -> None:
        entry_point = FakeEntryPoint("nope", "not a click command")
        original_commands = dict(isolated_cli.commands)

        registered = load_plugin_commands(isolated_cli, entry_points=[entry_point])

        assert registered == []
        assert isolated_cli.commands == original_commands
        warning = capsys.readouterr().err
        assert "Warning:" in warning
        assert "nope" in warning
        assert "not a click command" in warning
        assert warning.count("\n") == 1
        assert runner.invoke(isolated_cli, ["--help"]).exit_code == 0

    def test_no_plugins_leaves_cli_unchanged(
        self, runner: CliRunner, isolated_cli: click.Group
    ) -> None:
        before = runner.invoke(isolated_cli, ["--help"])
        original_commands = dict(isolated_cli.commands)

        registered = load_plugin_commands(isolated_cli, entry_points=[])

        assert registered == []
        assert isolated_cli.commands == original_commands
        after = runner.invoke(isolated_cli, ["--help"])
        assert after.exit_code == 0
        assert after.output == before.output

    def test_default_entry_points_are_discovered(
        self,
        monkeypatch: pytest.MonkeyPatch,
        isolated_cli: click.Group,
    ) -> None:
        @click.command()
        def extra() -> None:
            click.echo("extra")

        monkeypatch.setattr(
            plugins,
            "_installed_entry_points",
            lambda: [FakeEntryPoint("extra", extra)],
        )

        assert load_plugin_commands(isolated_cli) == ["extra"]
        assert "extra" in isolated_cli.commands
