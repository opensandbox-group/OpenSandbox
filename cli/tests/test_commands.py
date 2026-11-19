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

"""Tests for CLI commands with mocked SDK calls."""

from __future__ import annotations

import asyncio
import json
from contextlib import nullcontext
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest
from click.testing import CliRunner
from opensandbox.models.diagnostics import DiagnosticContent, DiagnosticContext
from opensandbox.models.execd import CommandLogs, Execution, ExecutionComplete
from opensandbox.models.sandboxes import (
    PagedSnapshotInfos,
    PaginationInfo,
    SandboxEndpoint,
    SandboxImageSpec,
    SnapshotInfo,
    SnapshotStatus,
)
from opensandbox.models.templates import (
    CreateTemplateRequest,
    PagedTemplateInfos,
    TemplateInfo,
    TemplateStatus,
)

from opensandbox_cli.commands.terminal import stream_pty
from opensandbox_cli.config import load_config_file
from opensandbox_cli.main import cli
from opensandbox_cli.output import OutputFormatter


@pytest.fixture()
def runner() -> CliRunner:
    return CliRunner()


def _build_mock_client_context(
    *,
    manager: MagicMock | None = None,
    sandbox: MagicMock | None = None,
    output_format: str = "json",
) -> MagicMock:
    ctx = MagicMock()
    ctx.resolved_config = {
        "api_key": "test-key",
        "domain": "localhost:8080",
        "protocol": "http",
        "request_timeout": 30,
        "use_server_proxy": False,
        "color": False,
        "default_image": None,
        "default_timeout": None,
    }
    ctx.config_path = Path("/tmp/mock-config.toml")
    ctx.cli_overrides = {
        "api_key": None,
        "domain": None,
        "protocol": None,
        "request_timeout": None,
        "use_server_proxy": None,
    }
    ctx.output = OutputFormatter(output_format, color=False)
    def _make_output(fmt: str) -> OutputFormatter:
        formatter = OutputFormatter(fmt, color=False)
        ctx.output = formatter
        return formatter
    ctx.make_output.side_effect = _make_output
    ctx.get_manager.return_value = manager or MagicMock()
    ctx.connect_sandbox.return_value = sandbox or MagicMock()
    ctx.connection_config = MagicMock()
    ctx.close = MagicMock()
    return ctx


def _invoke(
    runner: CliRunner,
    args: list[str],
    *,
    manager: MagicMock | None = None,
    sandbox: MagicMock | None = None,
    output_format: str = "json",
    input: str | None = None,
) -> object:
    """Invoke CLI with mocked ClientContext."""
    mock_ctx = _build_mock_client_context(
        manager=manager, sandbox=sandbox, output_format=output_format
    )

    with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
         patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx):
        mock_resolve.return_value = mock_ctx.resolved_config
        result = runner.invoke(cli, args, input=input, catch_exceptions=False)
    return result


class TestConfigInit:
    def test_init_creates_file(self, runner: CliRunner, tmp_path: Path) -> None:
        cfg_path = tmp_path / "config.toml"
        result = runner.invoke(cli, ["--config", str(cfg_path), "config", "init"])
        assert result.exit_code == 0
        assert "Config file created" in result.output


class TestConfigShow:
    def test_show_json_output(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["--api-key", "test-key", "config", "show", "-o", "json"])
        assert result.exit_code == 0
        data = json.loads(result.output)
        assert "api_key" in data
        assert data["api_key"] == "te****ey"
        assert data["config_path"].endswith(".opensandbox/config.toml")
        assert "config_file_exists" in data

    def test_show_table_output(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["--api-key", "test-key", "config", "show"])
        assert result.exit_code == 0
        assert "api_key" in result.output
        assert "test-key" not in result.output
        assert "te****ey" in result.output

    def test_global_request_timeout_flag_overrides_resolved_config(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["--request-timeout", "45", "config", "show", "-o", "json"])
        assert result.exit_code == 0
        data = json.loads(result.output)
        assert data["request_timeout"] == 45


class TestConfigSet:
    def test_set_uses_root_config_path(self, runner: CliRunner, tmp_path: Path) -> None:
        cfg_path = tmp_path / "custom.toml"
        runner.invoke(cli, ["--config", str(cfg_path), "config", "init"])

        result = runner.invoke(
            cli,
            ["--config", str(cfg_path), "config", "set", "connection.domain", "team.host"],
        )

        assert result.exit_code == 0
        assert 'domain = "team.host"' in cfg_path.read_text()

    def test_set_creates_missing_section(self, runner: CliRunner, tmp_path: Path) -> None:
        cfg_path = tmp_path / "config.toml"
        cfg_path.write_text("[connection]\n")

        result = runner.invoke(
            cli,
            ["--config", str(cfg_path), "config", "set", "defaults.image", "python:3.12"],
        )

        assert result.exit_code == 0
        parsed = load_config_file(cfg_path)
        assert parsed["defaults"]["image"] == "python:3.12"

    def test_set_infers_scalar_types(self, runner: CliRunner, tmp_path: Path) -> None:
        cfg_path = tmp_path / "config.toml"
        cfg_path.write_text("[connection]\n")

        runner.invoke(
            cli,
            ["--config", str(cfg_path), "config", "set", "connection.request_timeout", "45"],
        )
        runner.invoke(
            cli,
            ["--config", str(cfg_path), "config", "set", "connection.use_server_proxy", "true"],
        )

        parsed = load_config_file(cfg_path)
        assert parsed["connection"]["request_timeout"] == 45
        assert parsed["connection"]["use_server_proxy"] is True


# Sandbox commands


class TestSandboxList:
    def test_list_normalizes_state_filters_case_insensitively(self, runner: CliRunner) -> None:
        mock_mgr = MagicMock()
        mock_result = MagicMock()
        mock_result.sandbox_infos = []
        mock_mgr.list_sandbox_infos.return_value = mock_result

        result = _invoke(
            runner,
            ["sandbox", "list", "-o", "json", "--state", "running", "--state", "PAUSED"],
            manager=mock_mgr,
        )

        assert result.exit_code == 0
        filt = mock_mgr.list_sandbox_infos.call_args.args[0]
        assert filt.states == ["Running", "Paused"]

    def test_list_rejects_unknown_state_filter(self, runner: CliRunner) -> None:
        mock_mgr = MagicMock()
        result = _invoke(
            runner,
            ["sandbox", "list", "--state", "runing"],
            manager=mock_mgr,
        )

        assert result.exit_code != 0
        assert "Invalid sandbox state 'runing'" in result.output
        mock_mgr.list_sandbox_infos.assert_not_called()

    def test_list_passes_user_page_through_to_sdk(self, runner: CliRunner) -> None:
        mock_mgr = MagicMock()
        mock_result = MagicMock()
        mock_result.sandbox_infos = []
        mock_result.pagination.model_dump.return_value = {
            "page": 1,
            "page_size": 20,
            "total_items": 0,
            "total_pages": 0,
            "has_next_page": False,
        }
        mock_mgr.list_sandbox_infos.return_value = mock_result

        result = _invoke(
            runner,
            ["sandbox", "list", "--page", "1", "--page-size", "20", "-o", "json"],
            manager=mock_mgr,
        )

        assert result.exit_code == 0
        filt = mock_mgr.list_sandbox_infos.call_args.args[0]
        assert filt.page == 1
        data = json.loads(result.output)
        assert data["pagination"]["page"] == 1
        assert data["items"] == []


class TestSandboxCreate:
    def test_create_uses_config_defaults(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-123"
        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        mock_ctx.resolved_config["default_image"] = "python:3.12"
        mock_ctx.resolved_config["default_timeout"] = "15m"

        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(cli, ["sandbox", "create", "-o", "json"], catch_exceptions=False)

        assert result.exit_code == 0
        mock_create.assert_called_once()
        assert mock_create.call_args.args[0] == "python:3.12"
        assert mock_create.call_args.kwargs["timeout"].total_seconds() == 900

    def test_create_requires_image_when_no_default(self, runner: CliRunner) -> None:
        result = _invoke(runner, ["sandbox", "create"])
        assert result.exit_code != 0
        assert "Sandbox image is required" in result.output

    def test_create_supports_timeout_none(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-123"

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                ["sandbox", "create", "-o", "json", "--image", "python:3.12", "--timeout", "none"],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        assert mock_create.call_args.kwargs["timeout"] is None
        data = json.loads(result.output)
        assert data["timeout"] == "manual-cleanup"

    def test_create_reports_sdk_default_timeout_when_unset(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-123"

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                ["sandbox", "create", "-o", "json", "--image", "python:3.12"],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        assert "timeout" not in mock_create.call_args.kwargs
        data = json.loads(result.output)
        assert data["timeout"] == "sdk-default"

    def test_create_passes_image_auth_to_sdk(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-123"

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                [
                    "sandbox",
                    "create",
                    "-o",
                    "json",
                    "--image",
                    "private.example.com/team/app:latest",
                    "--image-auth-username",
                    "alice",
                    "--image-auth-password",
                    "secret-token",
                ],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        image_arg = mock_create.call_args.args[0]
        assert isinstance(image_arg, SandboxImageSpec)
        assert image_arg.image == "private.example.com/team/app:latest"
        assert image_arg.auth is not None
        assert image_arg.auth.username == "alice"
        assert image_arg.auth.password == "secret-token"

    def test_create_loads_volumes_from_file(self, runner: CliRunner, tmp_path: Path) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-123"
        volumes_path = tmp_path / "volumes.json"
        volumes_path.write_text(json.dumps([
            {
                "name": "workdir",
                "host": {"path": "/tmp/workdir"},
                "mountPath": "/workspace",
            }
        ]))

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                ["sandbox", "create", "-o", "json", "--image", "python:3.12", "--volumes-file", str(volumes_path)],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        mock_create.assert_called_once()
        volumes = mock_create.call_args.kwargs["volumes"]
        assert len(volumes) == 1
        assert volumes[0].name == "workdir"
        assert volumes[0].mount_path == "/workspace"

    def test_create_builds_entrypoint_argv_from_repeated_flags(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-123"

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                [
                    "sandbox",
                    "create",
                    "-o",
                    "json",
                    "--image",
                    "python:3.12",
                    "--entrypoint",
                    "python",
                    "--entrypoint",
                    "-m",
                    "--entrypoint",
                    "http.server",
                ],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        mock_create.assert_called_once()
        assert mock_create.call_args.kwargs["entrypoint"] == [
            "python",
            "-m",
            "http.server",
        ]

    def test_create_passes_extensions_to_sdk(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-123"

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                [
                    "sandbox",
                    "create",
                    "-o",
                    "json",
                    "--image",
                    "python:3.12",
                    "--extension",
                    "storage.id=abc123",
                    "--extension",
                    "runtime.profile=fast",
                ],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        mock_create.assert_called_once()
        assert mock_create.call_args.kwargs["extensions"] == {
            "storage.id": "abc123",
            "runtime.profile": "fast",
        }

    def test_create_passes_credential_proxy_to_sdk(
        self, runner: CliRunner, tmp_path: Path
    ) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-123"
        policy_path = tmp_path / "network-policy.json"
        policy_path.write_text(json.dumps({
            "defaultAction": "deny",
            "egress": [{"action": "allow", "target": "api.example.com"}],
        }))

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                [
                    "sandbox",
                    "create",
                    "-o",
                    "json",
                    "--image",
                    "python:3.12",
                    "--network-policy-file",
                    str(policy_path),
                    "--credential-proxy",
                ],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        assert mock_create.call_args.kwargs["network_policy"].egress[0].target == "api.example.com"
        assert mock_create.call_args.kwargs["credential_proxy"].enabled is True

    def test_create_from_template_calls_sdk(
        self, runner: CliRunner, tmp_path: Path
    ) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-tpl"
        policy_path = tmp_path / "network-policy.json"
        policy_path.write_text(json.dumps({
            "defaultAction": "deny",
            "egress": [{"action": "allow", "target": "api.example.com"}],
        }))

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create_from_template", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                [
                    "sandbox",
                    "create",
                    "-o",
                    "json",
                    "--template",
                    "tpl_abc",
                    "--timeout",
                    "30m",
                    "--metadata",
                    "team=infra",
                    "--extension",
                    "storage.id=abc123",
                    "--network-policy-file",
                    str(policy_path),
                ],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        mock_create.assert_called_once()
        assert mock_create.call_args.args[0] == "tpl_abc"
        kwargs = mock_create.call_args.kwargs
        assert kwargs["timeout"].total_seconds() == 1800
        assert kwargs["metadata"] == {"team": "infra"}
        assert kwargs["extensions"] == {"storage.id": "abc123"}
        assert kwargs["network_policy"].egress[0].target == "api.example.com"
        data = json.loads(result.output)
        assert data["template"] == "tpl_abc"

    def test_create_from_template_requires_timeout(self, runner: CliRunner) -> None:
        result = _invoke(runner, ["sandbox", "create", "--template", "tpl_abc"])
        assert result.exit_code != 0
        assert "--timeout is required" in result.output

    def test_create_from_snapshot_calls_sdk(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-snap"

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                ["sandbox", "create", "-o", "json", "--snapshot-id", "snap-1", "--timeout", "10m"],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        assert mock_create.call_args.args[0] is None
        assert mock_create.call_args.kwargs["snapshot_id"] == "snap-1"
        data = json.loads(result.output)
        assert data["snapshot_id"] == "snap-1"

    def test_create_from_file_maps_wire_request(
        self, runner: CliRunner, tmp_path: Path
    ) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-file"
        request_path = tmp_path / "sandbox-request.json"
        request_path.write_text(json.dumps({
            "image": "python:3.12",
            "timeout": 1800,
            "resourceLimits": {"cpu": "1", "memory": "2Gi"},
            "env": {"FOO": "bar"},
            "metadata": {"team": "infra"},
            "extensions": {"storage.id": "abc123"},
            "entrypoint": ["python", "-m", "http.server"],
        }))

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                ["sandbox", "create", "-o", "json", "-f", str(request_path)],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        mock_create.assert_called_once()
        assert mock_create.call_args.args[0] == "python:3.12"
        kwargs = mock_create.call_args.kwargs
        assert kwargs["timeout"].total_seconds() == 1800
        assert kwargs["resource"] == {"cpu": "1", "memory": "2Gi"}
        assert kwargs["env"] == {"FOO": "bar"}
        assert kwargs["metadata"] == {"team": "infra"}
        assert kwargs["extensions"] == {"storage.id": "abc123"}
        assert kwargs["entrypoint"] == ["python", "-m", "http.server"]
        data = json.loads(result.output)
        assert data["request_file"] == str(request_path)
        assert data["image"] == "python:3.12"

    def test_create_from_file_with_image_auth(
        self, runner: CliRunner, tmp_path: Path
    ) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-file"
        request_path = tmp_path / "sandbox-request.json"
        request_path.write_text(json.dumps({
            "image": {
                "uri": "private.example.com/team/app:latest",
                "auth": {"username": "alice", "password": "secret-token"},
            },
            "timeout": 600,
        }))

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                ["sandbox", "create", "-o", "json", "-f", str(request_path)],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        image_arg = mock_create.call_args.args[0]
        assert isinstance(image_arg, SandboxImageSpec)
        assert image_arg.image == "private.example.com/team/app:latest"
        assert image_arg.auth is not None
        assert image_arg.auth.username == "alice"
        assert image_arg.auth.password == "secret-token"

    def test_create_from_file_supports_manual_cleanup(
        self, runner: CliRunner, tmp_path: Path
    ) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-file"
        request_path = tmp_path / "sandbox-request.json"
        request_path.write_text(json.dumps({"image": "python:3.12", "timeout": None}))

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                ["sandbox", "create", "-o", "json", "-f", str(request_path)],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        assert mock_create.call_args.kwargs["timeout"] is None
        data = json.loads(result.output)
        assert data["timeout"] == "manual-cleanup"

    def test_create_from_file_with_template_calls_sdk(
        self, runner: CliRunner, tmp_path: Path
    ) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-tpl"
        request_path = tmp_path / "sandbox-request.json"
        request_path.write_text(json.dumps({
            "templateId": "tpl_abc",
            "timeout": 1800,
            "metadata": {"team": "infra"},
            "extensions": {"storage.id": "abc123"},
            "networkPolicy": {
                "defaultAction": "deny",
                "egress": [{"action": "allow", "target": "api.example.com"}],
            },
        }))

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create_from_template", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                ["sandbox", "create", "-o", "json", "-f", str(request_path)],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        mock_create.assert_called_once()
        assert mock_create.call_args.args[0] == "tpl_abc"
        kwargs = mock_create.call_args.kwargs
        assert kwargs["timeout"].total_seconds() == 1800
        assert kwargs["metadata"] == {"team": "infra"}
        assert kwargs["extensions"] == {"storage.id": "abc123"}
        assert kwargs["network_policy"].egress[0].target == "api.example.com"

    def test_create_from_file_rejects_payload_flags(
        self, runner: CliRunner, tmp_path: Path
    ) -> None:
        request_path = tmp_path / "sandbox-request.json"
        request_path.write_text(json.dumps({"image": "python:3.12"}))

        result = _invoke(
            runner,
            ["sandbox", "create", "-f", str(request_path), "--image", "python:3.11"],
        )
        assert result.exit_code != 0
        assert "--file cannot be combined with: --image" in result.output

    def test_create_from_file_applies_default_timeout(
        self, runner: CliRunner, tmp_path: Path
    ) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-file"
        request_path = tmp_path / "sandbox-request.json"
        request_path.write_text(json.dumps({"image": "python:3.12"}))

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        mock_ctx.resolved_config["default_timeout"] = "15m"
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                ["sandbox", "create", "-o", "json", "-f", str(request_path)],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        assert mock_create.call_args.kwargs["timeout"].total_seconds() == 900

    def test_create_from_file_allows_secure_access_false_with_template(
        self, runner: CliRunner, tmp_path: Path
    ) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-tpl"
        request_path = tmp_path / "sandbox-request.json"
        request_path.write_text(json.dumps({
            "templateId": "tpl_abc",
            "timeout": 600,
            "secureAccess": False,
        }))

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create_from_template", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                ["sandbox", "create", "-o", "json", "-f", str(request_path)],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        mock_create.assert_called_once()
        assert mock_create.call_args.args[0] == "tpl_abc"


def _make_template_info(phase: str = "Pending") -> TemplateInfo:
    return TemplateInfo(
        template_id="tpl_abc",
        image="python:3.12",
        publish="s3://bucket/publish",
        format="overlaybd",
        status=TemplateStatus(phase=phase),
        created_at=datetime(2026, 1, 1, tzinfo=timezone.utc),
        updated_at=datetime(2026, 1, 1, tzinfo=timezone.utc),
    )


class TestTemplateCommands:
    @pytest.mark.parametrize("publish", ["s3://bucket/publish", "oci://registry.example.com/team/templates"])
    def test_create_builds_request(self, runner: CliRunner, publish: str) -> None:
        mock_mgr = MagicMock()
        mock_mgr.create_template.return_value = _make_template_info()

        result = _invoke(
            runner,
            [
                "template",
                "create",
                "--image",
                "python:3.12",
                "--publish",
                publish,
                "--format",
                "overlaybd" if publish.startswith("oci://") else "native",
                "--resource",
                "cpu=1",
                "--entrypoint",
                "python",
                "--env",
                "FOO=bar",
                "--metadata",
                "team=infra",
                "--readiness-probe",
                "tcp://127.0.0.1:44772",
                "-o",
                "json",
            ],
            manager=mock_mgr,
        )

        assert result.exit_code == 0
        req = mock_mgr.create_template.call_args.args[0]
        assert isinstance(req, CreateTemplateRequest)
        assert req.image == "python:3.12"
        assert req.publish == publish
        assert req.format == ("overlaybd" if publish.startswith("oci://") else "native")
        assert req.resource_limits == {"cpu": "1"}
        assert req.entrypoint == ["python"]
        assert req.env == {"FOO": "bar"}
        assert req.metadata == {"team": "infra"}
        assert req.readiness is not None
        assert req.readiness.probe == "tcp://127.0.0.1:44772"
        data = json.loads(result.output)
        assert data["template_id"] == "tpl_abc"

    @pytest.mark.parametrize("publish", ["s3://bucket/publish", "oci://registry.example.com/team/templates"])
    def test_create_from_file_builds_request(
        self, runner: CliRunner, tmp_path: Path, publish: str
    ) -> None:
        mock_mgr = MagicMock()
        mock_mgr.create_template.return_value = _make_template_info()
        request_path = tmp_path / "template-request.json"
        request_path.write_text(json.dumps({
            "image": "python:3.12",
            "publish": publish,
            "format": "overlaybd" if publish.startswith("oci://") else "native",
            "resourceLimits": {"cpu": "1", "memory": "512Mi"},
            "entrypoint": ["python", "-m", "http.server"],
            "env": {"FOO": "bar"},
            "metadata": {"team": "infra"},
            "readiness": {"probe": "tcp://127.0.0.1:44772", "warmupSeconds": 90},
        }))

        result = _invoke(
            runner,
            ["template", "create", "-f", str(request_path), "-o", "json"],
            manager=mock_mgr,
        )

        assert result.exit_code == 0
        req = mock_mgr.create_template.call_args.args[0]
        assert isinstance(req, CreateTemplateRequest)
        assert req.image == "python:3.12"
        assert req.publish == publish
        assert req.format == ("overlaybd" if publish.startswith("oci://") else "native")
        assert req.resource_limits == {"cpu": "1", "memory": "512Mi"}
        assert req.entrypoint == ["python", "-m", "http.server"]
        assert req.env == {"FOO": "bar"}
        assert req.metadata == {"team": "infra"}
        assert req.readiness is not None
        assert req.readiness.probe == "tcp://127.0.0.1:44772"
        assert req.readiness.warmup_seconds == 90
        data = json.loads(result.output)
        assert data["request_file"] == str(request_path)

    def test_get_list_delete_use_manager(self, runner: CliRunner) -> None:
        mock_mgr = MagicMock()
        mock_mgr.get_template.return_value = _make_template_info()
        mock_mgr.list_templates.return_value = PagedTemplateInfos(
            template_infos=[_make_template_info()],
            pagination=PaginationInfo(
                page=1, page_size=20, total_items=1, total_pages=1, has_next_page=False
            ),
        )

        assert _invoke(runner, ["template", "get", "tpl_abc", "-o", "json"], manager=mock_mgr).exit_code == 0
        assert _invoke(runner, ["template", "list", "-o", "json"], manager=mock_mgr).exit_code == 0
        assert _invoke(runner, ["template", "delete", "tpl_abc"], manager=mock_mgr).exit_code == 0

        mock_mgr.get_template.assert_called_once_with("tpl_abc")
        mock_mgr.delete_template.assert_called_once_with("tpl_abc")
        mock_mgr.list_templates.assert_called_once()

    def test_get_table_includes_manifest_ref(self, runner: CliRunner) -> None:
        mock_mgr = MagicMock()
        info = _make_template_info("Succeeded")
        info.status = TemplateStatus(
            phase="Succeeded",
            manifest_ref="s3://bucket/publish/manifest.json",
        )
        mock_mgr.get_template.return_value = info

        result = _invoke(runner, ["template", "get", "tpl_abc"], manager=mock_mgr)

        assert result.exit_code == 0
        assert "s3://bucket/publish/manifest.json" in result.output


def _make_snapshot_info(state: str = "Succeeded") -> SnapshotInfo:
    return SnapshotInfo(
        id="snap-1",
        sandbox_id="sb-123",
        name="golden",
        status=SnapshotStatus(state=state),
        created_at=datetime(2026, 1, 1, tzinfo=timezone.utc),
    )


class TestSnapshotCommands:
    def test_create_calls_manager(self, runner: CliRunner) -> None:
        mock_mgr = MagicMock()
        mock_mgr.create_snapshot.return_value = _make_snapshot_info()

        result = _invoke(
            runner,
            ["snapshot", "create", "sb-123", "--name", "golden", "-o", "json"],
            manager=mock_mgr,
        )

        assert result.exit_code == 0
        mock_mgr.create_snapshot.assert_called_once_with("sb-123", name="golden")
        data = json.loads(result.output)
        assert data["id"] == "snap-1"

    def test_get_list_delete_use_manager(self, runner: CliRunner) -> None:
        mock_mgr = MagicMock()
        mock_mgr.get_snapshot.return_value = _make_snapshot_info()
        mock_mgr.list_snapshots.return_value = PagedSnapshotInfos(
            snapshot_infos=[_make_snapshot_info()],
            pagination=PaginationInfo(
                page=1, page_size=20, total_items=1, total_pages=1, has_next_page=False
            ),
        )

        assert _invoke(runner, ["snapshot", "get", "snap-1", "-o", "json"], manager=mock_mgr).exit_code == 0
        assert _invoke(
            runner,
            ["snapshot", "list", "--sandbox-id", "sb-123", "-o", "json"],
            manager=mock_mgr,
        ).exit_code == 0
        assert _invoke(runner, ["snapshot", "delete", "snap-1"], manager=mock_mgr).exit_code == 0

        mock_mgr.get_snapshot.assert_called_once_with("snap-1")
        mock_mgr.delete_snapshot.assert_called_once_with("snap-1")
        filt = mock_mgr.list_snapshots.call_args.args[0]
        assert filt.sandbox_id == "sb-123"


class TestSandboxKill:
    def test_kill_multiple(self, runner: CliRunner) -> None:
        mock_mgr = MagicMock()
        result = _invoke(runner, ["sandbox", "kill", "id1", "id2", "-o", "json"], manager=mock_mgr)
        assert result.exit_code == 0
        assert mock_mgr.kill_sandbox.call_count == 2
        data = json.loads(result.output)
        assert data == [
            {"sandbox_id": "id1", "status": "deletion_requested"},
            {"sandbox_id": "id2", "status": "deletion_requested"},
        ]


class TestSandboxPause:
    def test_pause_reports_request_accepted(self, runner: CliRunner) -> None:
        mock_mgr = MagicMock()
        result = _invoke(runner, ["sandbox", "pause", "sb-123"], manager=mock_mgr)
        assert result.exit_code == 0
        mock_mgr.pause_sandbox.assert_called_once_with("sb-123")
        assert "Pause request accepted: sb-123" in result.output


class TestSandboxResume:
    def test_resume_uses_sdk_resume_and_waits_for_readiness(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-123"

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.resume", return_value=mock_sb) as mock_resume:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(cli, ["sandbox", "resume", "sb-123"], catch_exceptions=False)

        assert result.exit_code == 0
        mock_resume.assert_called_once_with(
            "sb-123",
            connection_config=mock_ctx.connection_config,
            skip_health_check=False,
        )
        mock_sb.close.assert_called_once()
        assert "Sandbox resumed: sb-123" in result.output

    def test_resume_accepts_skip_health_check_and_timeout(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-123"

        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.resume", return_value=mock_sb) as mock_resume:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                ["sandbox", "resume", "sb-123", "--skip-health-check", "--resume-timeout", "45s"],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        mock_resume.assert_called_once_with(
            "sb-123",
            connection_config=mock_ctx.connection_config,
            skip_health_check=True,
            resume_timeout=timedelta(seconds=45),
        )
        mock_sb.close.assert_called_once()


class TestSandboxMetrics:
    def test_metrics_fetches_snapshot(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_metrics = MagicMock()
        mock_metrics.model_dump.return_value = {
            "cpu_count": 2,
            "cpu_used_percentage": 12.5,
            "memory_total_in_mib": 1024,
            "memory_used_in_mib": 256,
            "timestamp": 1710000000000,
        }
        mock_sb.get_metrics.return_value = mock_metrics

        result = _invoke(runner, ["sandbox", "metrics", "sb-1", "-o", "json"], sandbox=mock_sb)
        assert result.exit_code == 0
        data = json.loads(result.output)
        assert data["cpu_used_percentage"] == 12.5

    def test_metrics_watch_streams_json_samples(self, runner: CliRunner) -> None:
        class _FakeResponse:
            def __init__(self) -> None:
                self.lines = [
                    'data: {"cpu_count": 2, "cpu_used_pct": 12.5, "mem_total_mib": 1024, "mem_used_mib": 256, "timestamp": 1710000000000}',
                    "",
                    'data: {"cpu_count": 2, "cpu_used_pct": 18.0, "mem_total_mib": 1024, "mem_used_mib": 300, "timestamp": 1710000001000}',
                ]

            def __enter__(self) -> _FakeResponse:
                return self

            def __exit__(self, exc_type, exc, tb) -> None:
                return None

            def raise_for_status(self) -> None:
                return None

            def iter_lines(self):
                yield from self.lines

        mock_sb = MagicMock()
        mock_sb.metrics._httpx_client.stream.return_value = _FakeResponse()

        result = _invoke(
            runner,
            ["sandbox", "metrics", "sb-1", "--watch", "-o", "json"],
            sandbox=mock_sb,
        )
        assert result.exit_code == 0
        lines = [json.loads(line) for line in result.output.strip().splitlines()]
        assert len(lines) == 2
        assert lines[0]["cpu_used_percentage"] == 12.5
        assert lines[1]["memory_used_in_mib"] == 300


class TestSandboxEndpoint:
    def test_endpoint_passes_valid_port_to_sdk(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_endpoint = MagicMock()
        mock_endpoint.model_dump.return_value = {"endpoint": "http://example.test"}
        mock_sb.get_endpoint.return_value = mock_endpoint

        result = _invoke(
            runner,
            ["sandbox", "endpoint", "sb-1", "--port", "8080", "-o", "json"],
            sandbox=mock_sb,
        )

        assert result.exit_code == 0
        mock_sb.get_endpoint.assert_called_once_with(8080)


# File commands


class TestFileCat:
    def test_cat_outputs_content(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.files.read_file.return_value = "hello world"
        result = _invoke(
            runner,
            ["file", "cat", "sb-1", "/etc/hostname"],
            sandbox=mock_sb,
            output_format="table",
        )
        assert result.exit_code == 0
        assert "hello world" in result.output
        mock_sb.files.read_file.assert_called_once_with("/etc/hostname", encoding="utf-8")


class TestFileWrite:
    def test_write_with_content_flag(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        result = _invoke(
            runner,
            ["file", "write", "sb-1", "/tmp/test.txt", "-c", "content here"],
            sandbox=mock_sb,
        )
        assert result.exit_code == 0
        assert "Written" in result.output
        mock_sb.files.write_file.assert_called_once()

    def test_write_parses_permission_mode(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        result = _invoke(
            runner,
            ["file", "write", "sb-1", "/tmp/test.txt", "-c", "content here", "--mode", "644"],
            sandbox=mock_sb,
        )
        assert result.exit_code == 0
        mock_sb.files.write_file.assert_called_once_with(
            "/tmp/test.txt", "content here", encoding="utf-8", mode=644
        )


class TestFileTransfer:
    def test_upload_streams_file_object(self, runner: CliRunner, tmp_path: Path) -> None:
        mock_sb = MagicMock()
        local_path = tmp_path / "upload.bin"
        local_path.write_bytes(b"hello")

        result = _invoke(
            runner,
            ["file", "upload", "sb-1", str(local_path), "/tmp/upload.bin"],
            sandbox=mock_sb,
        )
        assert result.exit_code == 0
        uploaded = mock_sb.files.write_file.call_args.args[1]
        assert hasattr(uploaded, "read")
        assert not isinstance(uploaded, bytes)

    def test_download_streams_chunks_to_disk(self, runner: CliRunner, tmp_path: Path) -> None:
        mock_sb = MagicMock()
        mock_sb.files.read_bytes_stream.return_value = iter([b"hel", b"lo"])
        local_path = tmp_path / "nested" / "download.txt"

        result = _invoke(
            runner,
            ["file", "download", "sb-1", "/tmp/download.txt", str(local_path)],
            sandbox=mock_sb,
        )
        assert result.exit_code == 0
        assert local_path.read_bytes() == b"hello"
        mock_sb.files.read_bytes_stream.assert_called_once_with("/tmp/download.txt")

    def test_download_replaces_destination_after_stream_completes(
        self, runner: CliRunner, tmp_path: Path
    ) -> None:
        local_path = tmp_path / "result.json"
        original = b"previous successful result"
        local_path.write_bytes(original)

        def stream():
            for chunk in [b"new ", b"result"]:
                assert local_path.read_bytes() == original
                yield chunk
            assert local_path.read_bytes() == original

        mock_sb = MagicMock()
        mock_sb.files.read_bytes_stream.return_value = stream()
        result = _invoke(
            runner,
            ["file", "download", "sb-1", "/workspace/result.json", str(local_path)],
            sandbox=mock_sb,
        )

        assert result.exit_code == 0, result.output
        assert local_path.read_bytes() == b"new result"
        assert set(tmp_path.iterdir()) == {local_path}
        mock_sb.close.assert_called_once()


class TestFileRm:
    def test_rm_deletes_files(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        result = _invoke(
            runner, ["file", "rm", "sb-1", "/tmp/a", "/tmp/b", "-o", "json"], sandbox=mock_sb
        )
        assert result.exit_code == 0
        mock_sb.files.delete_files.assert_called_once_with(["/tmp/a", "/tmp/b"])
        data = json.loads(result.output)
        assert data == [
            {"path": "/tmp/a", "status": "deleted"},
            {"path": "/tmp/b", "status": "deleted"},
        ]


class TestFileMv:
    def test_mv_moves_file(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        result = _invoke(
            runner, ["file", "mv", "sb-1", "/tmp/old", "/tmp/new"], sandbox=mock_sb
        )
        assert result.exit_code == 0
        assert "Moved: /tmp/old" in result.output and "/tmp/new" in result.output
        entries = mock_sb.files.move_files.call_args.args[0]
        assert len(entries) == 1
        assert entries[0].src == "/tmp/old"
        assert entries[0].dest == "/tmp/new"


class TestFileMkdir:
    def test_mkdir_creates_dirs(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        result = _invoke(
            runner, ["file", "mkdir", "sb-1", "/tmp/dir1", "/tmp/dir2", "-o", "json"], sandbox=mock_sb
        )
        assert result.exit_code == 0
        data = json.loads(result.output)
        assert data == [
            {"path": "/tmp/dir1", "status": "created"},
            {"path": "/tmp/dir2", "status": "created"},
        ]

    def test_mkdir_parses_octal_mode(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        result = _invoke(
            runner,
            ["file", "mkdir", "sb-1", "/tmp/dir1", "--mode", "755"],
            sandbox=mock_sb,
        )
        assert result.exit_code == 0
        entry = mock_sb.files.create_directories.call_args.args[0][0]
        assert entry.mode == 755


class TestFileRmdir:
    def test_rmdir_removes_dirs(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        result = _invoke(
            runner, ["file", "rmdir", "sb-1", "/workspace/old", "-o", "json"], sandbox=mock_sb
        )
        assert result.exit_code == 0
        data = json.loads(result.output)
        assert data == [{"path": "/workspace/old", "status": "removed"}]


class TestFileInfo:
    def test_info_returns_one_aggregated_document(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        entry = MagicMock()
        entry.model_dump.return_value = {
            "mode": 644,
            "owner": "root",
            "group": "root",
            "size": 12,
            "created_at": "2026-01-01T00:00:00Z",
            "modified_at": "2026-01-02T00:00:00Z",
        }
        mock_sb.files.get_file_info.return_value = {
            "/tmp/a": entry,
            "/tmp/b": entry,
        }

        result = _invoke(
            runner,
            ["file", "info", "sb-1", "/tmp/a", "/tmp/b", "-o", "json"],
            sandbox=mock_sb,
        )

        assert result.exit_code == 0
        data = json.loads(result.output)
        assert [item["path"] for item in data] == ["/tmp/a", "/tmp/b"]


class TestFileChmod:
    def test_chmod_parses_permission_mode(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        result = _invoke(
            runner,
            ["file", "chmod", "sb-1", "/tmp/test.txt", "--mode", "755"],
            sandbox=mock_sb,
        )
        assert result.exit_code == 0
        entry = mock_sb.files.set_permissions.call_args.args[0][0]
        assert entry.mode == 755


class TestCommandSeparators:
    def test_command_run_supports_shell_payload_after_separator(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        execution = MagicMock()
        execution.error = None
        mock_sb.commands.run.return_value = execution

        result = _invoke(
            runner,
            ["command", "run", "sb-1", "--", "sh", "-lc", "echo ready"],
            sandbox=mock_sb,
            output_format="raw",
        )

        assert result.exit_code == 0
        mock_sb.commands.run.assert_called_once()
        assert mock_sb.commands.run.call_args.args[0] == "sh -lc 'echo ready'"

    def test_command_run_argv_flag_preserves_literal_arguments(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        execution = MagicMock()
        execution.error = None
        mock_sb.commands.run.return_value = execution

        result = _invoke(
            runner,
            [
                "command",
                "run",
                "sb-1",
                "--argv",
                "--",
                "python3",
                "-c",
                "import sys; print(sys.argv[1:])",
                "a b",
                "$HOME",
                "x'y",
                "",
            ],
            sandbox=mock_sb,
            output_format="raw",
        )

        assert result.exit_code == 0
        mock_sb.commands.run.assert_called_once()
        assert mock_sb.commands.run.call_args.args[0] == [
            "python3",
            "-c",
            "import sys; print(sys.argv[1:])",
            "a b",
            "$HOME",
            "x'y",
            "",
        ]

    def test_command_run_help_mentions_separator_rule(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["command", "run", "--help"])
        assert result.exit_code == 0
        assert "Separator rule: use `--` before the sandbox command payload." in result.output

    def test_session_run_help_mentions_separator_rule(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["command", "session", "run", "--help"])
        assert result.exit_code == 0
        assert "Separator rule: use `--` before the sandbox command payload." in result.output


# Egress commands


class TestEgressCommands:
    def test_get_prints_policy(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_policy = MagicMock()
        mock_policy.model_dump.return_value = {
            "defaultAction": "deny",
            "egress": [{"action": "allow", "target": "pypi.org"}],
        }
        mock_sb.get_egress_policy.return_value = mock_policy

        result = _invoke(runner, ["egress", "get", "sb-1", "-o", "json"], sandbox=mock_sb)
        assert result.exit_code == 0
        data = json.loads(result.output)
        assert data["defaultAction"] == "deny"

    def test_patch_calls_sdk(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-1"
        result = _invoke(
            runner,
            ["egress", "patch", "sb-1", "--rule", "allow=pypi.org", "--rule", "deny=bad.example.com", "-o", "json"],
            sandbox=mock_sb,
        )
        assert result.exit_code == 0
        mock_sb.patch_egress_rules.assert_called_once()
        rules = mock_sb.patch_egress_rules.call_args.args[0]
        assert len(rules) == 2
        assert rules[0].action == "allow"
        assert rules[0].target == "pypi.org"
        assert rules[1].action == "deny"
        assert rules[1].target == "bad.example.com"


# Credential Vault commands


class TestCredentialVaultCommands:
    def test_create_reads_payload_file_and_calls_sdk(
        self, runner: CliRunner, tmp_path: Path
    ) -> None:
        mock_sb = MagicMock()
        mock_state = MagicMock()
        mock_state.model_dump.return_value = {
            "revision": 1,
            "credentials": [{"name": "api-token", "source_type": "inline", "revision": 1}],
            "bindings": [],
        }
        mock_sb.credential_vault.create.return_value = mock_state
        payload_path = tmp_path / "vault.yaml"
        payload_path.write_text(
            """
credentials:
  - name: api-token
    source:
      value: secret-token
bindings: []
""".strip()
        )

        result = _invoke(
            runner,
            ["credential-vault", "create", "sb-1", "--file", str(payload_path), "-o", "json"],
            sandbox=mock_sb,
        )

        assert result.exit_code == 0
        mock_sb.credential_vault.create.assert_called_once_with(
            credentials=[
                {"name": "api-token", "source": {"value": "secret-token"}},
            ],
            bindings=[],
        )
        mock_sb.close.assert_called_once()
        assert "secret-token" not in result.output
        data = json.loads(result.output)
        assert data["revision"] == 1

    def test_create_reports_the_yaml_parser_error(
        self, runner: CliRunner, tmp_path: Path
    ) -> None:
        mock_sb = MagicMock()
        payload_path = tmp_path / "vault.yaml"
        payload_path.write_text("credentials: [\n")

        result = _invoke(
            runner,
            ["credential-vault", "create", "sb-1", "--file", str(payload_path)],
            sandbox=mock_sb,
        )

        assert result.exit_code != 0
        assert "line" in result.output
        assert str(payload_path) in result.output
        mock_sb.connect_sandbox.assert_not_called()
        mock_sb.credential_vault.create.assert_not_called()

    def test_get_calls_sdk(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_state = MagicMock()
        mock_state.model_dump.return_value = {
            "revision": 1,
            "credentials": [],
            "bindings": [],
        }
        mock_sb.credential_vault.get.return_value = mock_state

        result = _invoke(
            runner,
            ["credential-vault", "get", "sb-1", "-o", "json"],
            sandbox=mock_sb,
        )

        assert result.exit_code == 0
        mock_sb.credential_vault.get.assert_called_once_with()
        data = json.loads(result.output)
        assert data["credentials"] == []

    def test_patch_reads_stdin_and_calls_sdk(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_state = MagicMock()
        mock_state.model_dump.return_value = {
            "revision": 2,
            "credentials": [{"name": "runtime-token", "source_type": "inline", "revision": 2}],
            "bindings": [],
        }
        mock_sb.credential_vault.patch.return_value = mock_state
        payload = json.dumps({
            "expectedRevision": 1,
            "credentials": {
                "add": [
                    {
                        "name": "runtime-token",
                        "source": {"value": "runtime-secret"},
                    }
                ]
            },
        })

        result = _invoke(
            runner,
            ["credential-vault", "patch", "sb-1", "--file", "-", "-o", "json"],
            sandbox=mock_sb,
            input=payload,
        )

        assert result.exit_code == 0
        mock_sb.credential_vault.patch.assert_called_once_with(
            expected_revision=1,
            credentials={
                "add": [
                    {
                        "name": "runtime-token",
                        "source": {"value": "runtime-secret"},
                    }
                ]
            },
            bindings=None,
        )
        assert "runtime-secret" not in result.output
        data = json.loads(result.output)
        assert data["revision"] == 2

    def test_delete_calls_sdk(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-1"

        result = _invoke(
            runner,
            ["credential-vault", "delete", "sb-1", "-o", "json"],
            sandbox=mock_sb,
        )

        assert result.exit_code == 0
        mock_sb.credential_vault.delete.assert_called_once_with()
        data = json.loads(result.output)
        assert data == {"sandbox_id": "sb-1", "status": "deleted"}

    def test_credential_list_calls_sdk(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        credential = MagicMock()
        credential.model_dump.return_value = {
            "name": "api-token",
            "source_type": "inline",
            "revision": 1,
        }
        mock_sb.credential_vault.list_credentials.return_value = [credential]

        result = _invoke(
            runner,
            ["credential-vault", "credential", "list", "sb-1", "-o", "json"],
            sandbox=mock_sb,
        )

        assert result.exit_code == 0
        mock_sb.credential_vault.list_credentials.assert_called_once_with()
        data = json.loads(result.output)
        assert data[0]["name"] == "api-token"

    def test_binding_get_calls_sdk(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        binding = MagicMock()
        binding.model_dump.return_value = {
            "name": "api-binding",
            "revision": 1,
            "match": {"hosts": ["api.example.com"]},
            "auth": {"type": "apiKey", "name": "x-api-key"},
        }
        mock_sb.credential_vault.get_binding.return_value = binding

        result = _invoke(
            runner,
            ["credential-vault", "binding", "get", "sb-1", "api-binding", "-o", "json"],
            sandbox=mock_sb,
        )

        assert result.exit_code == 0
        mock_sb.credential_vault.get_binding.assert_called_once_with("api-binding")
        data = json.loads(result.output)
        assert data["auth"]["type"] == "apiKey"


# Command execution


class TestCommandRun:
    @pytest.mark.parametrize("output_format", ["table", "json", "yaml"])
    def test_background_run(self, runner: CliRunner, output_format: str) -> None:
        mock_sb = MagicMock()
        mock_execution = Execution(
            id="exec-123",
            complete=ExecutionComplete(timestamp=1, execution_time_in_millis=2),
        )
        mock_sb.commands.run.return_value = mock_execution

        result = _invoke(
            runner,
            ["command", "run", "sb-1", "-d", "-o", output_format, "--", "echo", "hello"],
            sandbox=mock_sb,
        )
        assert result.exit_code == 0
        if output_format == "json":
            data = json.loads(result.stdout)
            assert data["execution_id"] == "exec-123"
            assert data["mode"] == "background"
        else:
            assert "exec-123" in result.stdout
            assert "background" in result.stdout
        assert not result.stderr
        mock_sb.commands.run.assert_called_once()
        assert mock_sb.commands.run.call_args.args[0] == "echo hello"
        assert mock_sb.commands.run.call_args.kwargs["opts"].background is True
        mock_sb.commands.get_command_status.assert_not_called()
        mock_sb.close.assert_called_once()

    def test_background_run_argv_flag_passes_native_argv(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_execution = Execution(
            id="exec-123",
            complete=ExecutionComplete(timestamp=1, execution_time_in_millis=2),
        )
        mock_sb.commands.run.return_value = mock_execution

        result = _invoke(
            runner,
            ["command", "run", "sb-1", "-d", "--argv", "echo", "hello", "-o", "json"],
            sandbox=mock_sb,
        )
        assert result.exit_code == 0
        data = json.loads(result.output)
        assert data["execution_id"] == "exec-123"
        assert data["mode"] == "background"
        mock_sb.commands.run.assert_called_once()
        assert mock_sb.commands.run.call_args.args[0] == ["echo", "hello"]

    def test_foreground_run_rejects_json_output(self, runner: CliRunner) -> None:
        result = _invoke(
            runner,
            ["command", "run", "sb-1", "-o", "json", "--", "echo", "hello"],
        )
        assert result.exit_code != 0
        assert "Allowed values: raw" in result.output


class TestCommandLogs:
    def test_table_includes_next_cursor(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.commands.get_background_command_logs.return_value = CommandLogs(
            content="hello-from-exec",
            cursor=424242,
        )

        result = _invoke(
            runner, ["command", "logs", "sb-1", "exec-1"], sandbox=mock_sb
        )

        assert result.exit_code == 0
        assert "hello-from-exec" in result.output
        assert "424242" in result.output.split()

    def test_raw_logs_stay_the_payload(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.commands.get_background_command_logs.return_value = CommandLogs(
            content="hello-from-exec",
            cursor=424242,
        )

        result = _invoke(
            runner,
            ["command", "logs", "sb-1", "exec-1", "-o", "raw"],
            sandbox=mock_sb,
        )

        assert result.exit_code == 0
        assert "hello-from-exec" in result.output
        assert "424242" not in result.output


class TestCommandInterrupt:
    def test_interrupt_calls_sdk(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        result = _invoke(
            runner, ["command", "interrupt", "sb-1", "exec-789"], sandbox=mock_sb
        )
        assert result.exit_code == 0
        mock_sb.commands.interrupt.assert_called_once_with("exec-789")
        assert "Interrupted: exec-789" in result.output


class TestCommandSession:
    def test_session_create(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-1"
        mock_sb.commands.create_session.return_value = "sess-123"
        result = _invoke(
            runner,
            ["command", "session", "create", "sb-1", "--workdir", "/workspace", "-o", "json"],
            sandbox=mock_sb,
        )
        assert result.exit_code == 0
        data = json.loads(result.output)
        assert data["session_id"] == "sess-123"
        mock_sb.commands.create_session.assert_called_once_with(working_directory="/workspace")

    def test_session_run(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_execution = MagicMock()
        mock_execution.error = None
        mock_sb.commands.run_in_session.return_value = mock_execution
        result = _invoke(
            runner,
            ["command", "session", "run", "sb-1", "sess-123", "--timeout", "30s", "--", "pwd"],
            sandbox=mock_sb,
            output_format="table",
        )
        assert result.exit_code == 0
        mock_sb.commands.run_in_session.assert_called_once()
        assert mock_sb.commands.run_in_session.call_args.args[:2] == ("sess-123", "pwd")
        assert mock_sb.commands.run_in_session.call_args.kwargs["timeout"] == timedelta(seconds=30)

    def test_session_delete(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        result = _invoke(
            runner,
            ["command", "session", "delete", "sb-1", "sess-123"],
            sandbox=mock_sb,
        )
        assert result.exit_code == 0
        mock_sb.commands.delete_session.assert_called_once_with("sess-123")
        assert "Deleted session: sess-123" in result.output


# Stable diagnostics


class TestDiagnosticsCommands:
    def test_context_prints_structured_backend(self, runner: CliRunner) -> None:
        manager = MagicMock()
        manager.get_diagnostic_context.return_value = DiagnosticContext(
            sandboxId="sb-1", backendType="kubernetes",
            backendIdentifiers={"namespace": "tenant-ns"}, state="Pending",
        )
        result = _invoke(
            runner, ["diagnostics", "context", "sb-1", "-o", "json"], manager=manager,
        )
        assert result.exit_code == 0
        assert json.loads(result.output)["backend_identifiers"] == {"namespace": "tenant-ns"}
        manager.get_diagnostic_context.assert_called_once_with("sb-1")

    def test_logs_raw_prints_inline_content(self, runner: CliRunner) -> None:
        manager = MagicMock()
        manager.get_diagnostic_logs.return_value = DiagnosticContent(
            sandboxId="sb-1",
            kind="logs",
            scope="container",
            delivery="inline",
            contentType="text/plain; charset=utf-8",
            content="line 1\nline 2",
            truncated=False,
        )

        result = _invoke(
            runner,
            ["diagnostics", "logs", "sb-1", "--scope", "container", "-o", "raw"],
            manager=manager,
        )

        assert result.exit_code == 0
        assert "line 1\nline 2" in result.output
        manager.get_diagnostic_logs.assert_called_once_with("sb-1", scope="container")

    def test_events_json_prints_descriptor(self, runner: CliRunner) -> None:
        manager = MagicMock()
        manager.get_diagnostic_events.return_value = DiagnosticContent(
            sandboxId="sb-1",
            kind="events",
            scope="runtime",
            delivery="url",
            contentType="text/plain; charset=utf-8",
            contentUrl="https://example.com/events.txt",
            contentLength=12,
            truncated=False,
        )

        result = _invoke(
            runner,
            ["diagnostics", "events", "sb-1", "--scope", "runtime", "-o", "json"],
            manager=manager,
        )

        assert result.exit_code == 0
        data = json.loads(result.output)
        assert data["kind"] == "events"
        assert data["scope"] == "runtime"
        assert data["delivery"] == "url"
        assert data["content_url"] == "https://example.com/events.txt"
        assert data["content_length"] == 12
        manager.get_diagnostic_events.assert_called_once_with("sb-1", scope="runtime")

    def test_raw_url_delivery_prints_content_url(self, runner: CliRunner) -> None:
        manager = MagicMock()
        manager.get_diagnostic_logs.return_value = DiagnosticContent(
            sandboxId="sb-1",
            kind="logs",
            scope="container",
            delivery="url",
            contentType="text/plain; charset=utf-8",
            contentUrl="https://example.com/logs.txt",
            truncated=False,
        )

        result = _invoke(
            runner,
            ["diagnostics", "logs", "sb-1", "--scope", "container", "-o", "raw"],
            manager=manager,
        )

        assert result.exit_code == 0
        assert result.output.strip() == "https://example.com/logs.txt"


class _FakePTYWebSocket:
    """Minimal stand-in for a websockets client connection."""

    def __init__(self, incoming: list) -> None:
        self._incoming = list(incoming)
        self.sent: list = []

    async def __aenter__(self) -> _FakePTYWebSocket:
        return self

    async def __aexit__(self, *exc_info: object) -> bool:
        return False

    def __aiter__(self) -> _FakePTYWebSocket:
        return self

    async def __anext__(self) -> object:
        if not self._incoming:
            raise StopAsyncIteration
        return self._incoming.pop(0)

    async def send(self, data: object) -> None:
        self.sent.append(data)


def _replay_frame(payload: bytes, offset: int = 0) -> bytes:
    return b"\x03" + offset.to_bytes(8, "big") + payload


def _invoke_terminal(
    runner: CliRunner,
    args: list[str],
    *,
    incoming: list,
    stdin_chunks: list[bytes] | None = None,
    connect_error: Exception | None = None,
):
    """Run `osb terminal` with the execd PTY boundary fully mocked.

    Returns (result, fake_ws, create_mock, delete_mock, connect_calls, stdout).
    """
    sandbox = MagicMock()
    sandbox.get_endpoint.return_value = SandboxEndpoint(
        endpoint="execd.example:44772"
    )
    mock_ctx = _build_mock_client_context(sandbox=sandbox)
    connection = mock_ctx.connection_config
    connection.user_agent = "osb-test"
    connection.headers = {}
    connection.use_server_proxy = False
    connection.protocol = "http"
    connection.get_api_key.return_value = ""

    ws = _FakePTYWebSocket(incoming)
    stdout = bytearray()
    connect_calls: list = []
    chunks = list(stdin_chunks or [])

    async def fake_stdin_read() -> bytes:
        if chunks:
            return chunks.pop(0)
        return b""

    def fake_connect(url: str, additional_headers: dict | None = None):
        connect_calls.append((url, additional_headers))
        if connect_error is not None:
            raise connect_error
        return ws

    with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
         patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
         patch("opensandbox_cli.commands.terminal._require_posix_tty"), \
         patch(
             "opensandbox_cli.commands.terminal._posix_tty_io",
             return_value=(fake_stdin_read, stdout.extend, lambda: (80, 24), 0),
         ), \
         patch(
             "opensandbox_cli.commands.terminal._raw_mode",
             return_value=nullcontext(),
         ), \
         patch(
             "opensandbox_cli.commands.terminal._create_pty_session",
             return_value="sess-1",
         ) as create, \
         patch("opensandbox_cli.commands.terminal._delete_pty_session") as delete, \
         patch(
             "opensandbox_cli.commands.terminal.websockets.connect",
             side_effect=fake_connect,
         ):
        mock_resolve.return_value = mock_ctx.resolved_config
        result = runner.invoke(cli, args, catch_exceptions=False)
    return result, ws, create, delete, connect_calls, stdout


class TestTerminal:
    def test_renders_output_sends_stdin_and_exits_with_remote_code(
        self, runner: CliRunner
    ) -> None:
        incoming = [
            '{"type":"connected","mode":"pty","role":"holder"}',
            b"\x01hello",
            _replay_frame(b" world"),
            '{"type":"exit","exit_code":7}',
        ]

        result, ws, create, delete, connect_calls, stdout = _invoke_terminal(
            runner,
            ["terminal", "sbx-1"],
            incoming=incoming,
            stdin_chunks=[b"echo hi\n"],
        )

        assert result.exit_code == 7
        assert bytes(stdout) == b"hello world"
        assert json.loads(ws.sent[0]) == {"type": "resize", "cols": 80, "rows": 24}
        assert ws.sent[1] == b"\x00echo hi\n"
        create.assert_called_once()
        assert create.call_args.args[0] == "http://execd.example:44772"
        assert create.call_args.kwargs["workdir"] is None
        assert connect_calls == [
            ("ws://execd.example:44772/pty/sess-1/ws", {"User-Agent": "osb-test"})
        ]
        assert delete.call_args.args[2] == "sess-1"

    def test_workdir_is_forwarded_to_session_creation(
        self, runner: CliRunner
    ) -> None:
        incoming = ['{"type":"exit","exit_code":0}']

        result, _, create, _, _, _ = _invoke_terminal(
            runner,
            ["terminal", "sbx-1", "-w", "/tmp"],
            incoming=incoming,
        )

        assert result.exit_code == 0
        assert create.call_args.kwargs["workdir"] == "/tmp"


class TestStreamPTY:
    def test_error_frames_are_collected_and_exit_code_returned(self) -> None:
        async def scenario() -> tuple[int | None, list[str]]:
            ws = _FakePTYWebSocket(
                [
                    b"\x01out",
                    '{"type":"error","error":"boom"}',
                    '{"type":"exit","exit_code":3}',
                ]
            )

            async def stdin_read() -> bytes:
                return b""

            chunks = bytearray()
            return await stream_pty(
                ws, stdin_read=stdin_read, stdout_write=chunks.extend
            ), bytes(chunks)

        (exit_code, errors), written = asyncio.run(scenario())
        assert (exit_code, errors) == (3, ["boom"])
        assert written == b"out"

    def test_replay_frames_skip_offset_and_still_render(self) -> None:
        async def scenario() -> bytes:
            ws = _FakePTYWebSocket([_replay_frame(b"scrollback", offset=42)])

            async def stdin_read() -> bytes:
                return b""

            chunks = bytearray()
            await stream_pty(ws, stdin_read=stdin_read, stdout_write=chunks.extend)
            return bytes(chunks)

        assert asyncio.run(scenario()) == b"scrollback"
