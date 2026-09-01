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

"""Tests for the TOML config loader."""

import pytest

from config import _DEFAULTS, load_config, resolve_config_path


def write_config(tmp_path, content):
    path = tmp_path / "audit.toml"
    path.write_text(content, encoding="utf-8")
    return path


def test_missing_file_uses_defaults(tmp_path):
    config = load_config(tmp_path / "nonexistent.toml")

    assert config == _DEFAULTS


def test_config_overrides_defaults(tmp_path):
    path = write_config(
        tmp_path,
        """
[server]
host = "127.0.0.1"
port = 9000
ui_password = "secret"

[database]
url = "postgresql://audit@db:5432/audit"
pool_min = 2
pool_max = 20

[kubernetes]
kubeconfig = "/etc/audit/kubeconfig"
namespace = "opensandbox"
sync_interval = 300

[log]
level = "debug"
""",
    )

    config = load_config(path)

    assert config["host"] == "127.0.0.1"
    assert config["port"] == 9000
    assert config["ui_password"] == "secret"
    assert config["database_url"] == "postgresql://audit@db:5432/audit"
    assert config["db_pool_min"] == 2
    assert config["db_pool_max"] == 20
    assert config["kubeconfig"] == "/etc/audit/kubeconfig"
    assert config["k8s_namespace"] == "opensandbox"
    assert config["k8s_sync_interval"] == 300
    assert config["log_level"] == "debug"


def test_partial_config_keeps_other_defaults(tmp_path):
    path = write_config(tmp_path, '[database]\nurl = "postgresql://x"\n')

    config = load_config(path)

    assert config["database_url"] == "postgresql://x"
    assert config["host"] == _DEFAULTS["host"]
    assert config["ui_password"] == _DEFAULTS["ui_password"]


def test_unknown_keys_and_sections_ignored(tmp_path):
    path = write_config(
        tmp_path,
        '[server]\nhost = "0.0.0.0"\nfuture_key = 1\n\n[future_section]\nx = 2\n',
    )

    config = load_config(path)

    assert config["host"] == "0.0.0.0"


def test_malformed_toml_raises(tmp_path):
    path = write_config(tmp_path, "not [ valid toml")

    with pytest.raises(Exception):
        load_config(path)


def test_non_integer_port_raises(tmp_path):
    path = write_config(tmp_path, '[server]\nport = "eighty"\n')

    with pytest.raises(ValueError, match="port"):
        load_config(path)


def test_resolve_config_path_env_override(monkeypatch, tmp_path):
    monkeypatch.setenv("AUDIT_CONFIG_PATH", str(tmp_path / "custom.toml"))

    assert resolve_config_path() == tmp_path / "custom.toml"

    monkeypatch.delenv("AUDIT_CONFIG_PATH")
    assert resolve_config_path().name == "audit.toml"
