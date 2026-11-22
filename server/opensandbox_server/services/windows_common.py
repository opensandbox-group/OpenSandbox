# Copyright 2026 The OpenSandbox Authors
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
Shared Windows-profile helpers used by both Docker and Kubernetes runtimes.
"""

from __future__ import annotations

import math
import re
from typing import Optional, TYPE_CHECKING

from fastapi import HTTPException, status

from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.helpers import parse_nano_cpus

if TYPE_CHECKING:
    from opensandbox_server.api.schema import PlatformSpec

WINDOWS_USER_PORTS_ENV = "USER_PORTS"
WINDOWS_RAM_SIZE_ENV = "RAM_SIZE"
WINDOWS_CPU_CORES_ENV = "CPU_CORES"
WINDOWS_DISK_SIZE_ENV = "DISK_SIZE"
_WINDOWS_SIZE_PATTERN = re.compile(r"^\s*(\d+)\s*([a-zA-Z]*)\s*$")
# Size units the Windows profile understands. Gigabyte-class units are read as
# 1024-based, matching the dockur CPU_CORES/RAM_SIZE/DISK_SIZE values they are
# converted into. A bare number is bytes, which ResourceLimits documents as a
# valid memory/disk form.
_WINDOWS_SIZE_UNIT_BYTES = {
    "g": 1024**3,
    "gi": 1024**3,
    "gb": 1024**3,
    "m": 1024**2,
    "mi": 1024**2,
    "mb": 1024**2,
    "t": 1024**4,
    "ti": 1024**4,
    "tb": 1024**4,
}
WINDOWS_MIN_CPU_NANO_CPUS = 2_000_000_000
WINDOWS_MIN_MEMORY_GB = 4
WINDOWS_MIN_DISK_GB = 64


def is_windows_platform(platform: Optional["PlatformSpec"]) -> bool:
    return bool(platform and platform.os == "windows")


def inject_windows_user_ports(environment: list[str], exposed_ports: Optional[list[str]]) -> list[str]:
    """Ensure USER_PORTS includes container ports exposed for windows profile."""
    if not exposed_ports:
        return environment

    resolved_ports: list[str] = []
    for port_spec in exposed_ports:
        token = str(port_spec).split("/", 1)[0].strip()
        if token.isdigit() and token not in resolved_ports:
            resolved_ports.append(token)
    if not resolved_ports:
        return environment

    env_items = list(environment)
    user_ports_index: Optional[int] = None
    existing_ports: list[str] = []

    for idx, item in enumerate(env_items):
        if "=" not in item:
            continue
        key, value = item.split("=", 1)
        if key != WINDOWS_USER_PORTS_ENV:
            continue
        user_ports_index = idx
        existing_ports = [p.strip() for p in value.split(",") if p.strip()]
        break

    merged = list(existing_ports)
    for port in resolved_ports:
        if port not in merged:
            merged.append(port)
    merged_value = ",".join(merged)

    if user_ports_index is None:
        env_items.append(f"{WINDOWS_USER_PORTS_ENV}={merged_value}")
    else:
        env_items[user_ports_index] = f"{WINDOWS_USER_PORTS_ENV}={merged_value}"
    return env_items


def _windows_size_bytes(limit_value: Optional[str]) -> Optional[int]:
    """Parse a resourceLimits size into bytes; ``None`` when unrecognized."""
    if not limit_value:
        return None
    match = _WINDOWS_SIZE_PATTERN.match(limit_value)
    if not match:
        return None

    amount = int(match.group(1))
    unit = (match.group(2) or "").lower()
    if amount <= 0:
        return None
    if unit == "":
        return amount
    multiplier = _WINDOWS_SIZE_UNIT_BYTES.get(unit)
    if multiplier is None:
        return None
    return amount * multiplier


def _normalize_windows_size_env(limit_value: Optional[str]) -> Optional[str]:
    """Render a size as the dockur gigabyte env value, rounding up to whole GiB."""
    size_bytes = _windows_size_bytes(limit_value)
    if size_bytes is None:
        return None
    return f"{max(1, math.ceil(size_bytes / 1024**3))}G"


def _windows_size_meets_minimum(limit_value: Optional[str], minimum_gb: int) -> bool:
    """Compare the requested size, not the ceil'd dockur gigabyte env value."""
    size_bytes = _windows_size_bytes(limit_value)
    if size_bytes is None:
        return False
    return size_bytes >= minimum_gb * 1024**3


def inject_windows_resource_limits_env(
    environment: list[str],
    resource_limits: Optional[dict[str, str]],
) -> list[str]:
    """Convert resourceLimits into dockur/windows resource envs.

    resourceLimits takes precedence over user-supplied env values when present.
    """
    if not resource_limits:
        return environment

    env_items = list(environment)

    def upsert_env(key: str, value: str) -> None:
        for idx, item in enumerate(env_items):
            if "=" not in item:
                continue
            existing_key, _ = item.split("=", 1)
            if existing_key == key:
                env_items[idx] = f"{key}={value}"
                return
        env_items.append(f"{key}={value}")

    cpu_limit = resource_limits.get("cpu")
    nano_cpus = parse_nano_cpus(cpu_limit)
    if nano_cpus:
        cores = max(1, math.ceil(nano_cpus / 1_000_000_000))
        upsert_env(WINDOWS_CPU_CORES_ENV, str(cores))

    memory_limit = resource_limits.get("memory")
    normalized_memory = _normalize_windows_size_env(memory_limit)
    if normalized_memory:
        upsert_env(WINDOWS_RAM_SIZE_ENV, normalized_memory)

    disk_limit = (
        resource_limits.get("disk")
        or resource_limits.get("storage")
        or resource_limits.get("ephemeral-storage")
    )
    normalized_disk = _normalize_windows_size_env(disk_limit)
    if normalized_disk:
        upsert_env(WINDOWS_DISK_SIZE_ENV, normalized_disk)

    return env_items


def validate_windows_resource_limits(resource_limits: Optional[dict[str, str]]) -> None:
    """Validate minimum resource limits for windows profile."""
    if not resource_limits:
        return

    cpu_limit = resource_limits.get("cpu")
    if cpu_limit is not None:
        nano_cpus = parse_nano_cpus(cpu_limit)
        if nano_cpus is None or nano_cpus < WINDOWS_MIN_CPU_NANO_CPUS:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": (
                        "Windows profile requires resourceLimits.cpu >= 2 "
                        f"(received: {cpu_limit})."
                    ),
                },
            )

    memory_limit = resource_limits.get("memory")
    if memory_limit is not None:
        if not _windows_size_meets_minimum(memory_limit, WINDOWS_MIN_MEMORY_GB):
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": (
                        "Windows profile requires resourceLimits.memory >= 4G "
                        f"(received: {memory_limit})."
                    ),
                },
            )

    disk_limit = (
        resource_limits.get("disk")
        or resource_limits.get("storage")
        or resource_limits.get("ephemeral-storage")
    )
    if disk_limit is not None:
        if not _windows_size_meets_minimum(disk_limit, WINDOWS_MIN_DISK_GB):
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": (
                        "Windows profile requires resourceLimits.disk (or storage/ephemeral-storage) >= 64G "
                        f"(received: {disk_limit})."
                    ),
                },
            )
