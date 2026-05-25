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

"""Tests for CLI commands with mocked SDK calls.

Strategy: patch ``opensandbox_cli.main.ClientContext`` and ``resolve_config``
so the root ``cli`` callback creates our mock instead of a real SDK client.
"""

from __future__ import annotations

import json
from datetime import timedelta
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest
from click.testing import CliRunner
from opensandbox.models.diagnostics import DiagnosticContent
from opensandbox.models.sandboxes import SandboxImageSpec

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
    ctx.resolve_sandbox_id.side_effect = lambda prefix: prefix  # passthrough
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
) -> object:
    """Invoke CLI with mocked ClientContext."""
    mock_ctx = _build_mock_client_context(
        manager=manager, sandbox=sandbox, output_format=output_format
    )

    with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
         patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx):
        mock_resolve.return_value = mock_ctx.resolved_config
        result = runner.invoke(cli, args, catch_exceptions=False)
    return result


# ---------------------------------------------------------------------------
# Config commands (no SDK mocking needed)
# ---------------------------------------------------------------------------


class TestConfigInit:
    def test_init_creates_file(self, runner: CliRunner, tmp_path: Path) -> None:
        cfg_path = tmp_path / "config.toml"
        result = runner.invoke(cli, ["--config", str(cfg_path), "config", "init"])
        assert result.exit_code == 0
        assert "Config file created" in result.output

    def test_init_refuses_overwrite(self, runner: CliRunner, tmp_path: Path) -> None:
        cfg_path = tmp_path / "config.toml"
        cfg_path.write_text("existing")
        result = runner.invoke(cli, ["--config", str(cfg_path), "config", "init"])
        assert "already exists" in result.output

    def test_init_force_overwrites(self, runner: CliRunner, tmp_path: Path) -> None:
        cfg_path = tmp_path / "config.toml"
        cfg_path.write_text("old")
        result = runner.invoke(cli, ["--config", str(cfg_path), "config", "init", "--force"])
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
    def test_set_updates_existing_field(self, runner: CliRunner, tmp_path: Path) -> None:
        cfg_path = tmp_path / "config.toml"
        runner.invoke(cli, ["--config", str(cfg_path), "config", "init"])
        result = runner.invoke(
            cli,
            ["--config", str(cfg_path), "config", "set", "connection.domain", "new.host"],
        )
        assert result.exit_code == 0
        assert "Set connection.domain = new.host" in result.output

    def test_set_rejects_flat_key(self, runner: CliRunner, tmp_path: Path) -> None:
        cfg_path = tmp_path / "config.toml"
        cfg_path.write_text("[connection]\n")
        result = runner.invoke(
            cli,
            ["--config", str(cfg_path), "config", "set", "flat_key", "value"],
        )
        assert result.exit_code != 0
        assert "section.field" in result.output

    def test_set_uses_root_config_path(self, runner: CliRunner, tmp_path: Path) -> None:
        cfg_path = tmp_path / "custom.toml"
        runner.invoke(cli, ["--config", str(cfg_path), "config", "init"])

        result = runner.invoke(
            cli,
            ["--config", str(cfg_path), "config", "set", "connection.domain", "team.host"],
        )

        assert result.exit_code == 0
        assert 'domain = "team.host"' in cfg_path.read_text()

    def test_set_fails_when_config_file_is_missing(self, runner: CliRunner, tmp_path: Path) -> None:
        cfg_path = tmp_path / "missing.toml"
        result = runner.invoke(
            cli,
            ["--config", str(cfg_path), "config", "set", "connection.domain", "team.host"],
        )
        assert result.exit_code != 0
        assert "Run 'osb config init' first." in result.output


# ---------------------------------------------------------------------------
# Sandbox commands
# ---------------------------------------------------------------------------


class TestSandboxList:
    def test_list_invokes_manager(self, runner: CliRunner) -> None:
        mock_mgr = MagicMock()
        mock_result = MagicMock()
        mock_result.sandbox_infos = []
        mock_mgr.list_sandbox_infos.return_value = mock_result

        result = _invoke(runner, ["sandbox", "list", "-o", "json"], manager=mock_mgr)
        assert result.exit_code == 0
        mock_mgr.list_sandbox_infos.assert_called_once()

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

    def test_list_help_uses_one_indexed_pages(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["sandbox", "list", "--help"])
        assert result.exit_code == 0
        assert "Page number (1-indexed)." in result.output

    def test_list_rejects_page_zero(self, runner: CliRunner) -> None:
        result = _invoke(runner, ["sandbox", "list", "--page", "0"])
        assert result.exit_code != 0
        assert "0 is not in the range x>=1" in result.output

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

    def test_create_supports_default_timeout_none(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_sb.id = "sb-123"
        mock_ctx = _build_mock_client_context(sandbox=mock_sb)
        mock_ctx.resolved_config["default_image"] = "python:3.12"
        mock_ctx.resolved_config["default_timeout"] = "none"

        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx), \
             patch("opensandbox.sync.sandbox.SandboxSync.create", return_value=mock_sb) as mock_create:
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(cli, ["sandbox", "create", "-o", "json"], catch_exceptions=False)

        assert result.exit_code == 0
        assert mock_create.call_args.kwargs["timeout"] is None

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

    def test_create_requires_both_image_auth_fields(self, runner: CliRunner) -> None:
        result = _invoke(
            runner,
            [
                "sandbox",
                "create",
                "--image",
                "private.example.com/team/app:latest",
                "--image-auth-username",
                "alice",
            ],
        )
        assert result.exit_code != 0
        assert "Pass both --image-auth-username and --image-auth-password together." in result.output

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


class TestSandboxKill:
    def test_kill_multiple(self, runner: CliRunner) -> None:
        mock_mgr = MagicMock()
        result = _invoke(runner, ["sandbox", "kill", "id1", "id2", "-o", "json"], manager=mock_mgr)
        assert result.exit_code == 0
        assert mock_mgr.kill_sandbox.call_count == 2
        data = json.loads(result.output)
        assert data == [
            {"sandbox_id": "id1", "status": "terminated"},
            {"sandbox_id": "id2", "status": "terminated"},
        ]


class TestSandboxPause:
    def test_pause_calls_manager(self, runner: CliRunner) -> None:
        mock_mgr = MagicMock()
        result = _invoke(runner, ["sandbox", "pause", "sb-123"], manager=mock_mgr)
        assert result.exit_code == 0
        mock_mgr.pause_sandbox.assert_called_once_with("sb-123")
        assert "Sandbox paused: sb-123" in result.output


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
                    'data: {"cpu_count": 2, "cpu_used_percentage": 12.5, "memory_total_in_mib": 1024, "memory_used_in_mib": 256, "timestamp": 1710000000000}',
                    "",
                    'data: {"cpu_count": 2, "cpu_used_percentage": 18.0, "memory_total_in_mib": 1024, "memory_used_in_mib": 300, "timestamp": 1710000001000}',
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

    def test_metrics_watch_warns_and_continues_on_error_events(self, runner: CliRunner) -> None:
        class _FakeResponse:
            def __init__(self) -> None:
                self.lines = [
                    'data: {"cpu_count": 2, "cpu_used_percentage": 12.5, "memory_total_in_mib": 1024, "memory_used_in_mib": 256, "timestamp": 1710000000000}',
                    'data: {"error": "failed to get CPU percent"}',
                    'data: {"cpu_count": 2, "cpu_used_percentage": 18.0, "memory_total_in_mib": 1024, "memory_used_in_mib": 300, "timestamp": 1710000001000}',
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
        decoder = json.JSONDecoder()
        items: list[dict[str, object]] = []
        raw = result.output.strip()
        index = 0
        while index < len(raw):
            while index < len(raw) and raw[index].isspace():
                index += 1
            if index >= len(raw):
                break
            item, next_index = decoder.raw_decode(raw, index)
            items.append(item)
            index = next_index

        assert len(items) == 3
        assert items[0]["cpu_used_percentage"] == 12.5
        assert items[1]["status"] == "warning"
        assert items[1]["message"] == "Metrics stream error: failed to get CPU percent"
        assert items[2]["cpu_used_percentage"] == 18.0


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

    def test_endpoint_rejects_invalid_port(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        result = _invoke(
            runner,
            ["sandbox", "endpoint", "sb-1", "--port", "70000"],
            sandbox=mock_sb,
        )

        assert result.exit_code != 0
        assert "70000 is not in the range 1<=x<=65535" in result.output
        mock_sb.get_endpoint.assert_not_called()


# ---------------------------------------------------------------------------
# File commands
# ---------------------------------------------------------------------------


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

    def test_cat_rejects_json_output(self, runner: CliRunner) -> None:
        result = _invoke(runner, ["file", "cat", "sb-1", "/etc/hostname", "-o", "json"])
        assert result.exit_code != 0
        assert "Invalid value for '-o' / '--output'" in result.output


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

    def test_command_run_help_mentions_separator_rule(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["command", "run", "--help"])
        assert result.exit_code == 0
        assert "Separator rule: use `--` before the sandbox command payload." in result.output

    def test_session_run_help_mentions_separator_rule(self, runner: CliRunner) -> None:
        result = runner.invoke(cli, ["command", "session", "run", "--help"])
        assert result.exit_code == 0
        assert "Separator rule: use `--` before the sandbox command payload." in result.output


# ---------------------------------------------------------------------------
# Egress commands
# ---------------------------------------------------------------------------


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


# ---------------------------------------------------------------------------
# Command execution
# ---------------------------------------------------------------------------


class TestCommandRun:
    def test_background_run(self, runner: CliRunner) -> None:
        mock_sb = MagicMock()
        mock_execution = MagicMock()
        mock_execution.id = "exec-123"
        mock_sb.commands.run.return_value = mock_execution

        result = _invoke(
            runner,
            ["command", "run", "sb-1", "-d", "echo", "hello", "-o", "json"],
            sandbox=mock_sb,
        )
        assert result.exit_code == 0
        data = json.loads(result.output)
        assert data["execution_id"] == "exec-123"
        assert data["mode"] == "background"

    def test_foreground_run_rejects_json_output(self, runner: CliRunner) -> None:
        result = _invoke(
            runner,
            ["command", "run", "sb-1", "-o", "json", "--", "echo", "hello"],
        )
        assert result.exit_code != 0
        assert "Allowed values: raw" in result.output


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

    def test_session_run_rejects_json_output(self, runner: CliRunner) -> None:
        result = _invoke(
            runner,
            ["command", "session", "run", "sb-1", "sess-123", "-o", "json", "--", "pwd"],
        )
        assert result.exit_code != 0
        assert "Invalid value for '-o' / '--output'" in result.output


# ---------------------------------------------------------------------------
# DevOps diagnostics
# ---------------------------------------------------------------------------


class TestDevopsCommands:
    def test_logs_preserves_legacy_plain_text_filters_with_warning(self, runner: CliRunner) -> None:
        mock_ctx = _build_mock_client_context()
        response = MagicMock()
        response.status_code = 200
        response.text = "sandbox logs"
        response.raise_for_status = MagicMock()
        devops_client = MagicMock()
        devops_client.get.return_value = response
        mock_ctx.get_devops_client.return_value = devops_client

        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx):
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                ["devops", "logs", "sb-1", "--tail", "100", "--since", "30m"],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        assert "deprecated" in result.output
        assert "sandbox logs" in result.output
        devops_client.get.assert_called_once_with(
            "sandboxes/sb-1/diagnostics/logs",
            params={"tail": 100, "since": "30m"},
        )

    def test_events_preserves_legacy_limit_with_warning(self, runner: CliRunner) -> None:
        mock_ctx = _build_mock_client_context()
        response = MagicMock()
        response.status_code = 200
        response.text = "sandbox events"
        response.raise_for_status = MagicMock()
        devops_client = MagicMock()
        devops_client.get.return_value = response
        mock_ctx.get_devops_client.return_value = devops_client

        with patch("opensandbox_cli.main.resolve_config") as mock_resolve, \
             patch("opensandbox_cli.main.ClientContext", return_value=mock_ctx):
            mock_resolve.return_value = mock_ctx.resolved_config
            result = runner.invoke(
                cli,
                ["devops", "events", "sb-1", "--limit", "25"],
                catch_exceptions=False,
            )

        assert result.exit_code == 0
        assert "deprecated" in result.output
        assert "sandbox events" in result.output
        devops_client.get.assert_called_once_with(
            "sandboxes/sb-1/diagnostics/events",
            params={"limit": 25},
        )


# ---------------------------------------------------------------------------
# Stable diagnostics
# ---------------------------------------------------------------------------


class TestDiagnosticsCommands:
    def test_logs_requires_scope(self, runner: CliRunner) -> None:
        result = _invoke(runner, ["diagnostics", "logs", "sb-1", "-o", "raw"])
        assert result.exit_code != 0
        assert "Missing option '--scope'" in result.output

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
