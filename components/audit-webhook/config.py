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

"""TOML configuration for the audit webhook.

Settings are read from a TOML file (``audit.toml`` next to ``main.py`` by
default, override with the ``AUDIT_CONFIG_PATH`` environment variable).
A missing file means "use defaults"; a malformed file fails startup.
See ``audit.toml.example`` for the recognized keys.
"""

import logging
import os
import tomllib
from pathlib import Path

logger = logging.getLogger(__name__)

# Points at the config file; the only setting that stays an env var.
CONFIG_PATH_ENV = "AUDIT_CONFIG_PATH"

DEFAULT_CONFIG_PATH = Path(__file__).resolve().parent / "audit.toml"

_DEFAULTS = {
    "host": "0.0.0.0",
    "port": 8080,
    "ui_password": "",
    "database_url": "postgresql://postgres:postgres@localhost:5432/opensandbox_audit",
    "db_pool_min": 1,
    "db_pool_max": 10,
    "kubeconfig": "",
    "k8s_namespace": "",
    "k8s_sync_interval": 0,
    "log_level": "INFO",
}

_INT_KEYS = ("port", "db_pool_min", "db_pool_max", "k8s_sync_interval")

# TOML section -> flat config keys.
_SECTION_KEYS = {
    "server": {"host": "host", "port": "port", "ui_password": "ui_password"},
    "database": {
        "url": "database_url",
        "pool_min": "db_pool_min",
        "pool_max": "db_pool_max",
    },
    "kubernetes": {
        "kubeconfig": "kubeconfig",
        "namespace": "k8s_namespace",
        "sync_interval": "k8s_sync_interval",
    },
    "log": {"level": "log_level"},
}


def resolve_config_path(path: str | Path | None = None) -> Path:
    """Config file location: explicit path, then ``AUDIT_CONFIG_PATH``,
    then ``audit.toml`` next to this file."""
    if path is not None:
        return Path(path).expanduser()
    env_path = os.environ.get(CONFIG_PATH_ENV)
    if env_path:
        return Path(env_path).expanduser()
    return DEFAULT_CONFIG_PATH


def load_config(path: str | Path | None = None) -> dict:
    """Load settings from the TOML config file, falling back to defaults.

    Unknown keys are ignored so new sections can be added without breaking
    older deployments. Raises on an unreadable or malformed file - a
    present-but-broken config should not silently run with defaults.
    """
    config_path = resolve_config_path(path)
    data = {}
    if not config_path.exists():
        logger.info("config file %s not found; using defaults", config_path)
    else:
        try:
            with config_path.open("rb") as fh:
                data = tomllib.load(fh)
            logger.info("loaded configuration from %s", config_path)
        except Exception:
            logger.exception("failed to read config file %s", config_path)
            raise

    config = dict(_DEFAULTS)
    for section, keys in _SECTION_KEYS.items():
        section_data = data.get(section, {})
        if not isinstance(section_data, dict):
            raise ValueError(
                f"config section [{section}] must be a table, got "
                f"{type(section_data).__name__}"
            )
        for toml_key, config_key in keys.items():
            if toml_key in section_data:
                config[config_key] = section_data[toml_key]

    for key in _INT_KEYS:
        try:
            config[key] = int(config[key])
        except (TypeError, ValueError):
            raise ValueError(f"config key {key!r} must be an integer") from None

    return config
