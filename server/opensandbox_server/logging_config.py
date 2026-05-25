# Copyright 2025 Alibaba Group Holding Ltd.
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

"""
Logging configuration for the OpenSandbox server.

Two output modes:

  stdout (default)
    All loggers write to stdout via uvicorn's ColorizingFormatter,
    which supports %(levelprefix)s (colored, padded level name).

  file  (when log_cfg.file_enabled is true)
    All loggers write to a rotating file.  %(levelprefix)s is replaced
    with %(levelname)-8s because the standard logging.Formatter does not
    inject that uvicorn-only field.

    Optionally, uvicorn.access can be separated into its own rotating file
    (log_cfg.access_file_path) following the Nginx/Gunicorn convention.

urllib3 InsecureRequestWarning is silenced at startup because it comes from
expected unverified HTTPS calls (e.g. in-cluster k8s API) and produces
high-frequency noise without actionable content.
"""

import copy
import logging
import logging.config
from pathlib import Path

import urllib3
from uvicorn.config import LOGGING_CONFIG as UVICORN_LOGGING_CONFIG

from opensandbox_server.config import LogConfig

# %(levelprefix)s: colored, padded label injected by uvicorn's ColorizingFormatter.
_STDOUT_FMT = "%(levelprefix)s %(asctime)s [%(request_id)s] %(name)s: %(message)s"
# %(levelname)-8s: plain, left-aligned label (width=8) for the standard Formatter.
_FILE_FMT = "%(levelname)-8s %(asctime)s [%(request_id)s] %(name)s: %(message)s"
_DATEFMT = "%Y-%m-%d %H:%M:%S%z"

# dictConfig factory string for RequestIdFilter.
_REQUEST_ID_FILTER = {"()": "opensandbox_server.middleware.request_id.RequestIdFilter"}


def _rotating_file_handler(filename: str, log_cfg: LogConfig) -> dict:
    """Return a dictConfig handler entry for a RotatingFileHandler."""
    return {
        "class": "logging.handlers.RotatingFileHandler",
        "formatter": "file",
        "filters": ["request_id"],
        "filename": filename,
        "maxBytes": log_cfg.file_max_bytes,
        "backupCount": log_cfg.file_backup_count,
        "encoding": "utf-8",
    }


def _apply_stdout_config(log_config: dict, level: str) -> None:
    """Patch uvicorn's stdout handlers/formatters with unified format and request_id filter."""
    for name in ("default", "access"):
        log_config["formatters"][name]["fmt"] = _STDOUT_FMT
        log_config["formatters"][name]["datefmt"] = _DATEFMT
        log_config["formatters"][name]["use_colors"] = True

    log_config["handlers"]["default"]["filters"] = ["request_id"]
    log_config["handlers"]["access"]["filters"] = ["request_id"]

    log_config["loggers"]["opensandbox_server"] = {
        "handlers": ["default"],
        "level": level,
        "propagate": False,
    }


def _apply_file_config(log_config: dict, log_cfg: LogConfig, level: str) -> None:
    """
    Register file handlers and route all loggers to them.

    uvicorn.access is routed to access_file when resolved_access_file_path() returns
    a path, otherwise it shares the main file handler with the rest.
    """
    # Use resolved paths (defaults applied when file_enabled=true and paths not set).
    file_path = log_cfg.resolved_file_path()
    access_file_path = log_cfg.resolved_access_file_path()

    # Ensure parent directories exist.
    Path(file_path).parent.mkdir(parents=True, exist_ok=True)

    log_config["formatters"]["file"] = {"format": _FILE_FMT, "datefmt": _DATEFMT}
    log_config["handlers"]["file"] = _rotating_file_handler(file_path, log_cfg)

    if access_file_path:
        Path(access_file_path).parent.mkdir(parents=True, exist_ok=True)
        log_config["handlers"]["access_file"] = _rotating_file_handler(
            access_file_path, log_cfg
        )
        access_handler = "access_file"
    else:
        access_handler = "file"

    for logger_name in ("uvicorn", "uvicorn.error", "opensandbox_server"):
        log_config["loggers"].setdefault(logger_name, {})
        log_config["loggers"][logger_name]["handlers"] = ["file"]
        log_config["loggers"][logger_name]["propagate"] = False

    log_config["loggers"].setdefault("uvicorn.access", {})
    log_config["loggers"]["uvicorn.access"]["handlers"] = [access_handler]
    log_config["loggers"]["uvicorn.access"]["propagate"] = False

    log_config["loggers"]["opensandbox_server"]["level"] = level


def configure_logging(log_cfg: LogConfig) -> dict:
    """
    Build and apply the server logging configuration.

    Returns the final dictConfig dict for reuse by callers (e.g. uvicorn.run).
    """
    # Silence high-frequency noise from expected unverified HTTPS calls.
    urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

    level = log_cfg.level.upper()
    log_config = copy.deepcopy(UVICORN_LOGGING_CONFIG)

    # Register the request_id filter so any handler can reference it by name.
    log_config["filters"] = {"request_id": _REQUEST_ID_FILTER}

    if log_cfg.resolved_file_path():
        _apply_file_config(log_config, log_cfg, level)
    else:
        _apply_stdout_config(log_config, level)

    logging.config.dictConfig(log_config)
    logging.getLogger().setLevel(getattr(logging, level, logging.INFO))

    return log_config
