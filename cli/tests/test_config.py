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

"""Tests for opensandbox_cli.config — config loading and priority merging."""

from __future__ import annotations

from pathlib import Path

import pytest

from opensandbox_cli.config import (
    DEFAULT_CONFIG_TEMPLATE,
    init_config_file,
    load_config_file,
    resolve_config,
)

# ---------------------------------------------------------------------------
# load_config_file
# ---------------------------------------------------------------------------


class TestLoadConfigFile:
    def test_returns_empty_when_file_missing(self, tmp_path: Path) -> None:
        result = load_config_file(tmp_path / "nonexistent.toml")
        assert result == {}

    def test_parses_toml_file(self, tmp_path: Path) -> None:
        cfg = tmp_path / "config.toml"
        cfg.write_text(
            '[connection]\napi_key = "abc"\ndomain = "example.com"\n'
        )
        result = load_config_file(cfg)
        assert result["connection"]["api_key"] == "abc"
        assert result["connection"]["domain"] == "example.com"

    def test_parses_all_sections(self, tmp_path: Path) -> None:
        cfg = tmp_path / "config.toml"
        cfg.write_text(
            '[connection]\napi_key = "k"\n\n'
            '[output]\ncolor = false\n\n'
            '[defaults]\nimage = "alpine"\ntimeout = "5m"\n'
        )
        result = load_config_file(cfg)
        assert result["output"]["color"] is False
        assert result["defaults"]["image"] == "alpine"
        assert result["defaults"]["timeout"] == "5m"


# ---------------------------------------------------------------------------
# resolve_config — priority: CLI > env > file > defaults
# ---------------------------------------------------------------------------


class TestResolveConfig:
    def test_defaults_when_nothing_configured(self, tmp_path: Path) -> None:
        cfg_path = tmp_path / "empty.toml"
        cfg_path.write_text("")
        result = resolve_config(config_path=cfg_path)
        assert result["api_key"] is None
        assert result["domain"] is None
        assert result["protocol"] == "http"
        assert result["request_timeout"] == 30
        assert result["use_server_proxy"] is False
        assert result["color"] is True

    def test_file_values_override_defaults(self, tmp_path: Path) -> None:
        cfg = tmp_path / "config.toml"
        cfg.write_text(
            '[connection]\napi_key = "file-key"\ndomain = "file.host"\n'
            'protocol = "https"\nrequest_timeout = 60\nuse_server_proxy = true\n\n'
            '[output]\ncolor = false\n\n'
            '[defaults]\nimage = "node:20"\ntimeout = "15m"\n'
        )
        result = resolve_config(config_path=cfg)
        assert result["api_key"] == "file-key"
        assert result["domain"] == "file.host"
        assert result["protocol"] == "https"
        assert result["request_timeout"] == 60
        assert result["use_server_proxy"] is True
        assert result["color"] is False
        assert result["default_image"] == "node:20"
        assert result["default_timeout"] == "15m"

    def test_env_overrides_file(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        cfg = tmp_path / "config.toml"
        cfg.write_text('[connection]\napi_key = "file-key"\ndomain = "file.host"\n')

        monkeypatch.setenv("OPEN_SANDBOX_API_KEY", "env-key")
        monkeypatch.setenv("OPEN_SANDBOX_DOMAIN", "env.host")
        monkeypatch.setenv("OPEN_SANDBOX_PROTOCOL", "https")
        monkeypatch.setenv("OPEN_SANDBOX_REQUEST_TIMEOUT", "120")
        monkeypatch.setenv("OPEN_SANDBOX_USE_SERVER_PROXY", "true")

        result = resolve_config(config_path=cfg)
        assert result["api_key"] == "env-key"
        assert result["domain"] == "env.host"
        assert result["protocol"] == "https"
        assert result["request_timeout"] == 120
        assert result["use_server_proxy"] is True

    def test_cli_overrides_everything(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        cfg = tmp_path / "config.toml"
        cfg.write_text('[connection]\napi_key = "file-key"\n')
        monkeypatch.setenv("OPEN_SANDBOX_API_KEY", "env-key")

        result = resolve_config(
            cli_api_key="cli-key",
            cli_domain="cli.host",
            cli_protocol="https",
            cli_timeout=999,
            cli_use_server_proxy=True,
            config_path=cfg,
        )
        assert result["api_key"] == "cli-key"
        assert result["domain"] == "cli.host"
        assert result["protocol"] == "https"
        assert result["request_timeout"] == 999
        assert result["use_server_proxy"] is True

    def test_invalid_timeout_env_falls_through(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        cfg = tmp_path / "empty.toml"
        cfg.write_text("")
        monkeypatch.setenv("OPEN_SANDBOX_REQUEST_TIMEOUT", "not-a-number")
        result = resolve_config(config_path=cfg)
        # Falls through to default 30
        assert result["request_timeout"] == 30

    def test_invalid_bool_env_falls_through(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        cfg = tmp_path / "empty.toml"
        cfg.write_text("")
        monkeypatch.setenv("OPEN_SANDBOX_USE_SERVER_PROXY", "not-a-bool")
        result = resolve_config(config_path=cfg)
        assert result["use_server_proxy"] is False


# ---------------------------------------------------------------------------
# init_config_file
# ---------------------------------------------------------------------------


class TestInitConfigFile:
    def test_creates_default_config(self, tmp_path: Path) -> None:
        cfg_path = tmp_path / ".opensandbox" / "config.toml"
        result = init_config_file(cfg_path)
        assert result == cfg_path
        assert cfg_path.exists()
        content = cfg_path.read_text()
        assert "[connection]" in content
        assert "[output]" in content
        assert "[defaults]" in content

    def test_refuses_overwrite_without_force(self, tmp_path: Path) -> None:
        cfg_path = tmp_path / "config.toml"
        cfg_path.write_text("existing")
        with pytest.raises(FileExistsError, match="already exists"):
            init_config_file(cfg_path)

    def test_force_overwrites(self, tmp_path: Path) -> None:
        cfg_path = tmp_path / "config.toml"
        cfg_path.write_text("old content")
        init_config_file(cfg_path, force=True)
        assert cfg_path.read_text() == DEFAULT_CONFIG_TEMPLATE

    def test_creates_parent_directories(self, tmp_path: Path) -> None:
        cfg_path = tmp_path / "a" / "b" / "c" / "config.toml"
        init_config_file(cfg_path)
        assert cfg_path.exists()
