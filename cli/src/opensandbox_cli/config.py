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

"""CLI configuration loading and management.

Priority (highest to lowest):
  1. CLI flags
  2. Environment variables
  3. Config file (~/.opensandbox/config.toml)
  4. SDK defaults
"""

from __future__ import annotations

import os
import sys
from pathlib import Path
from typing import Any

import click

if sys.version_info >= (3, 11):
    import tomllib
else:
    try:
        import tomli as tomllib  # type: ignore[no-redef]
    except ModuleNotFoundError:  # pragma: no cover
        tomllib = None  # type: ignore[assignment]


DEFAULT_CONFIG_DIR = Path.home() / ".opensandbox"
DEFAULT_CONFIG_PATH = DEFAULT_CONFIG_DIR / "config.toml"

DEFAULT_CONFIG_TEMPLATE = """\
# OpenSandbox CLI configuration
# Priority: CLI flags > environment variables > this file > SDK defaults

[connection]
# api_key = "your-api-key"
# domain = "localhost:8080"
# protocol = "http"
# request_timeout = 30
# use_server_proxy is deprecated (maintenance mode); prefer the ingress gateway for data-plane traffic.
# use_server_proxy = false

[output]
# color = true

[defaults]
# image = "python:3.11"
# timeout = "10m"  # or "none" for manual cleanup mode
"""


def load_config_file(config_path: Path | None = None) -> dict[str, Any]:
    """Load the TOML config file; return {} when it does not exist."""
    path = config_path or DEFAULT_CONFIG_PATH
    if tomllib is None:
        return {}
    # Open directly: exists() raises on an unsearchable parent directory.
    try:
        with open(path, "rb") as f:
            return tomllib.load(f)
    except (FileNotFoundError, NotADirectoryError):
        return {}
    except OSError as exc:
        raise click.ClickException(f"Cannot read config file {path}: {exc}") from exc
    except (tomllib.TOMLDecodeError, UnicodeDecodeError) as exc:
        raise click.ClickException(
            f"Config file is not valid TOML ({path}): {exc}"
        ) from exc


# Read as text and stored verbatim so an API key like "007123" is not
# rewritten as the number 7123.
STRING_KEYS = frozenset(
    {
        "connection.api_key",
        "connection.domain",
        "connection.protocol",
        "defaults.image",
        "defaults.timeout",
    }
)


def resolve_config(
    *,
    cli_api_key: str | None = None,
    cli_domain: str | None = None,
    cli_protocol: str | None = None,
    cli_timeout: int | None = None,
    cli_use_server_proxy: bool | None = None,
    config_path: Path | None = None,
) -> dict[str, Any]:
    """Merge config from all sources into a flat dict.

    Config-file values are coerced to each key's declared type. Keys returned:
    api_key, domain, protocol, request_timeout (int seconds),
    use_server_proxy (bool; deprecated), color, default_image, default_timeout.
    """
    file_cfg = load_config_file(config_path)
    for section in ("connection", "output", "defaults"):
        if section in file_cfg and not isinstance(file_cfg[section], dict):
            raise click.ClickException(
                f"Config section '{section}' must be a TOML table (use [{section}])."
            )
    conn = file_cfg.get("connection", {})
    output_cfg = file_cfg.get("output", {})
    defaults = file_cfg.get("defaults", {})

    return {
        "api_key": cli_api_key
        or os.getenv("OPEN_SANDBOX_API_KEY")
        or _str_or_none(conn.get("api_key")),
        "domain": cli_domain
        or os.getenv("OPEN_SANDBOX_DOMAIN")
        or _str_or_none(conn.get("domain")),
        "protocol": cli_protocol
        or os.getenv("OPEN_SANDBOX_PROTOCOL")
        or _str_or_none(conn.get("protocol"))
        or "http",
        "request_timeout": _request_timeout_seconds(
            cli_timeout,
            _int_or_none(os.getenv("OPEN_SANDBOX_REQUEST_TIMEOUT")),
            _int_or_none(conn.get("request_timeout")),
        ),
        "use_server_proxy": _coalesce(
            cli_use_server_proxy,
            _bool_or_none(os.getenv("OPEN_SANDBOX_USE_SERVER_PROXY")),
            _bool_or_none(conn.get("use_server_proxy")),
            False,
        ),
        "color": _coalesce(_bool_or_none(output_cfg.get("color")), True),
        "default_image": _str_or_none(defaults.get("image")),
        "default_timeout": _str_or_none(defaults.get("timeout")),
    }


def init_config_file(config_path: Path | None = None, *, force: bool = False) -> Path:
    """Create a default config file. Returns the path written."""
    path = config_path or DEFAULT_CONFIG_PATH
    if path.exists() and not force:
        raise FileExistsError(
            f"Config file already exists at {path}. Use --force to overwrite."
        )
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(DEFAULT_CONFIG_TEMPLATE)
    return path


def _request_timeout_seconds(*candidates: int | None) -> int:
    """Return the first explicit timeout in seconds; zero/negative values are rejected rather than ignored."""
    for value in candidates:
        if value is None:
            continue
        if value <= 0:
            raise click.ClickException(
                "Request timeout must be a positive number of seconds, "
                f"got {value}."
            )
        return value
    return 30


def _int_or_none(value: Any) -> int | None:
    """Coerce a configured value to ``int``, or ``None`` when it is not numeric."""
    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    if isinstance(value, (str, float)):
        try:
            return int(value)
        except (TypeError, ValueError, OverflowError):
            return None
    return None


def _bool_or_none(value: Any) -> bool | None:
    """Coerce to ``bool``, accepting 0/1 so hand-edited files keep resolving."""
    if value is None:
        return None
    if isinstance(value, bool):
        return value
    if isinstance(value, (int, float)):
        return bool(value) if value in (0, 1) else None
    if not isinstance(value, str):
        return None
    normalized = value.strip().lower()
    if normalized in ("1", "true", "yes", "on"):
        return True
    if normalized in ("0", "false", "no", "off"):
        return False
    return None


def _str_or_none(value: Any) -> str | None:
    """Coerce a configured value to ``str``, or ``None`` when it has no text form."""
    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, str):
        return value
    if isinstance(value, (int, float)):
        return str(value)
    return None


def _coalesce(*values: Any) -> Any:
    for value in values:
        if value is not None:
            return value
    return None
