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

"""CLI output must survive streams that cannot encode emoji.

The CLI prints emoji and box-drawing characters, which a legacy Windows code
page (cp936, cp1252, ...) or a POSIX ``C`` locale cannot represent. Rich and
Click used to raise ``UnicodeEncodeError`` and the command exited 1 with a
traceback after already doing its work. ``CliRunner(charset=...)`` reproduces
those stream encodings on any host.

Streams that cannot represent a character escape it (``backslashreplace``)
instead of substituting ``?``, so payload and machine-readable output stay
recoverable.
"""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Any
from unittest.mock import MagicMock, patch

import pytest
import yaml
from click.testing import CliRunner, Result
from opensandbox.models.execd import OutputMessage

from opensandbox_cli import main as cli_main
from opensandbox_cli.main import cli
from opensandbox_cli.output import OutputFormatter


def _gbk_runner() -> CliRunner:
    """Return a runner whose captured output uses a legacy Windows code page."""
    return CliRunner(charset="gbk")


class TestNonUtf8Streams:
    @pytest.mark.parametrize("charset", ["gbk", "cp1252"])
    def test_help_output_is_not_aborted(self, charset: str) -> None:
        runner = CliRunner(charset=charset)

        root_help = runner.invoke(cli, ["--help"])
        assert root_help.exit_code == 0
        assert "Usage:" in root_help.output

        config_help = runner.invoke(cli, ["config", "--help"])
        assert config_help.exit_code == 0
        assert "Manage CLI configuration" in config_help.output

    def test_config_messages_are_not_aborted(self, tmp_path: Path) -> None:
        runner = _gbk_runner()
        cfg_path = tmp_path / "config.toml"

        init = runner.invoke(cli, ["--config", str(cfg_path), "config", "init"])
        assert init.exit_code == 0
        assert cfg_path.exists()
        assert "Config file created" in init.output

        set_result = runner.invoke(
            cli,
            ["--config", str(cfg_path), "config", "set", "connection.domain", "team.host"],
        )
        assert set_result.exit_code == 0
        assert 'domain = "team.host"' in cfg_path.read_text(encoding="utf-8")

    def test_utf8_streams_still_render_emoji(self, tmp_path: Path) -> None:
        result = CliRunner().invoke(
            cli, ["--config", str(tmp_path / "config.toml"), "config", "init"]
        )

        assert result.exit_code == 0
        assert "\u2705" in result.output

    def test_yaml_output_round_trips_on_gbk(self, tmp_path: Path) -> None:
        runner = _gbk_runner()
        cfg_path = tmp_path / "config.toml"
        runner.invoke(cli, ["--config", str(cfg_path), "config", "init"])
        value = "img-\u2705"
        set_result = runner.invoke(
            cli,
            ["--config", str(cfg_path), "config", "set", "defaults.image", value],
        )
        assert set_result.exit_code == 0

        result = runner.invoke(
            cli, ["--config", str(cfg_path), "config", "show", "-o", "yaml"]
        )

        assert result.exit_code == 0
        assert "\u2705" not in result.output
        assert yaml.safe_load(result.output)["default_image"] == value

    def test_real_errors_still_fail(self, tmp_path: Path) -> None:
        result = _gbk_runner().invoke(
            cli,
            [
                "--config",
                str(tmp_path / "missing.toml"),
                "config",
                "set",
                "connection.domain",
                "team.host",
            ],
        )

        assert result.exit_code != 0
        assert "Config file not found" in result.output


def _invoke_on_gbk(args: list[str], sandbox: MagicMock) -> Result:
    """Run the CLI against a mocked SDK with a gbk output stream."""
    ctx = MagicMock()
    ctx.resolved_config = {"color": False}
    ctx.make_output.side_effect = lambda fmt: OutputFormatter(fmt, color=False)
    ctx.connect_sandbox.return_value = sandbox
    runner = CliRunner(charset="gbk")
    with patch("opensandbox_cli.main.resolve_config", return_value={}), patch(
        "opensandbox_cli.main.ClientContext", return_value=ctx
    ):
        return runner.invoke(cli, args, catch_exceptions=False)


class TestPayloadOutputIsLossless:
    """Payload writes must not degrade to ``?`` on non-UTF-8 streams."""

    def test_file_cat_streams_payload_without_loss(self) -> None:
        sandbox = MagicMock()
        sandbox.files.read_file.return_value = "file \u2705"
        result = _invoke_on_gbk(["file", "cat", "sb-1", "/note.txt"], sandbox)

        assert result.exit_code == 0
        assert result.output == "file \\u2705"

    def test_command_streaming_output_without_loss(self) -> None:
        sandbox = MagicMock()

        def streamed(
            payload: str, *, opts: Any = None, handlers: Any = None
        ) -> MagicMock:
            handlers.on_stdout(OutputMessage(text="ready \u2705\n", timestamp=0))
            handlers.on_stderr(OutputMessage(text="warn \u2705\n", timestamp=0))
            execution = MagicMock()
            execution.error = None
            return execution

        sandbox.commands.run.side_effect = streamed
        result = _invoke_on_gbk(
            ["command", "run", "sb-1", "-o", "raw", "--", "echo", "ready"], sandbox
        )

        assert result.exit_code == 0
        assert "ready \\u2705" in result.output
        assert "warn \\u2705" in result.output
        assert "?" not in result.output


class _RecordingStream:
    """Stand-in for a text stream whose error handler can be reconfigured."""

    def __init__(self, encoding: str) -> None:
        self.encoding = encoding
        self.errors: str | None = None
        self.reconfigured: list[dict[str, str]] = []

    def reconfigure(self, **kwargs: str) -> None:
        self.reconfigured.append(kwargs)


def test_only_unencodable_streams_are_reconfigured(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    stdout = _RecordingStream("utf-8")
    stderr = _RecordingStream("gbk")
    monkeypatch.setattr(sys, "stdout", stdout)
    monkeypatch.setattr(sys, "stderr", stderr)

    cli_main._configure_output_streams()

    assert stdout.reconfigured == []
    assert stderr.reconfigured == [{"errors": "backslashreplace"}]


