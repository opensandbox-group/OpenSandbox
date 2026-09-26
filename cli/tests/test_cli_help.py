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

"""Tests that all CLI commands register correctly and --help exits cleanly."""

from __future__ import annotations

import pytest
from click.testing import CliRunner

from opensandbox_cli.main import cli


@pytest.fixture()
def runner() -> CliRunner:
    return CliRunner()


class TestRootCLI:
    def test_help_lists_options_and_commands(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["--help"])
        assert result.exit_code == 0
        assert "OpenSandbox CLI" in result.output
        assert "--request-timeout" in result.output
        assert "--timeout" not in result.output
        for cmd in (
            "sandbox",
            "template",
            "snapshot",
            "command",
            "file",
            "egress",
            "credential-vault",
            "config",
            "diagnostics",
            "devops",
            "skills",
        ):
            assert cmd in result.output

    def test_version(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["--version"])
        assert result.exit_code == 0
        assert "opensandbox" in result.output


class TestSandboxHelp:
    def test_sandbox_help(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["sandbox", "--help"])
        assert result.exit_code == 0
        for subcmd in ("create", "list", "get", "kill", "pause", "resume", "renew", "endpoint", "health", "metrics"):
            assert subcmd in result.output


class TestTemplateAndSnapshotHelp:
    @pytest.mark.parametrize("group", ["template", "snapshot"])
    def test_group_help_lists_subcommands(self, runner: CliRunner, group: str) -> None:
        result = runner.invoke(cli, [group, "--help"])
        assert result.exit_code == 0
        for subcmd in ("create", "get", "list", "delete"):
            assert subcmd in result.output


class TestCommandHelp:
    def test_command_help(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["command", "--help"])
        assert result.exit_code == 0
        for subcmd in ("run", "status", "logs", "interrupt", "session"):
            assert subcmd in result.output

    def test_command_session_help(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["command", "session", "--help"])
        assert result.exit_code == 0
        for subcmd in ("create", "run", "delete"):
            assert subcmd in result.output


class TestFileHelp:
    def test_file_help(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["file", "--help"])
        assert result.exit_code == 0
        for subcmd in ("cat", "write", "upload", "download", "rm", "mv", "mkdir", "rmdir", "search", "info", "chmod", "replace"):
            assert subcmd in result.output


class TestEgressHelp:
    def test_egress_help(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["egress", "--help"])
        assert result.exit_code == 0
        for subcmd in ("get", "patch"):
            assert subcmd in result.output


class TestCredentialVaultHelp:
    def test_credential_vault_help(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["credential-vault", "--help"])
        assert result.exit_code == 0
        for subcmd in ("create", "get", "patch", "delete", "credential", "binding"):
            assert subcmd in result.output

    @pytest.mark.parametrize("group", ["credential", "binding"])
    def test_credential_vault_subgroup_help(self, runner: CliRunner, group: str) -> None:
        result = runner.invoke(cli, ["credential-vault", group, "--help"])
        assert result.exit_code == 0
        for subcmd in ("list", "get"):
            assert subcmd in result.output


class TestConfigHelp:
    def test_config_help(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["config", "--help"])
        assert result.exit_code == 0
        for subcmd in ("init", "show", "set"):
            assert subcmd in result.output


class TestDevopsHelp:
    def test_devops_help(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["devops", "--help"])
        assert result.exit_code == 0
        for subcmd in ("logs", "inspect", "events", "summary"):
            assert subcmd in result.output


class TestDiagnosticsHelp:
    def test_diagnostics_help_documents_scopes(self, runner: CliRunner) -> None:
        logs = runner.invoke(cli, ["diagnostics", "logs", "--help"])
        events = runner.invoke(cli, ["diagnostics", "events", "--help"])
        assert logs.exit_code == 0
        assert events.exit_code == 0
        assert "--scope" in logs.output
        assert "container" in logs.output
        assert "content URL" in logs.output
        assert "lifecycle" not in logs.output
        assert "--scope" in events.output
        assert "runtime" in events.output
        assert "content URL" in events.output
        assert "lifecycle" not in events.output


class TestSkillsHelp:
    def test_skills_help(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["skills", "--help"])
        assert result.exit_code == 0
        for subcmd in ("install", "show", "list", "uninstall"):
            assert subcmd in result.output
