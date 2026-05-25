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
Shared helpers for container-based sandbox services.

These utilities centralize common parsing, filtering, and transformation logic
so multiple container runtimes (docker, kubernetes, etc.) can reuse them.
"""

from __future__ import annotations

import logging
import re
from datetime import datetime, timezone
from typing import Dict, Optional

from opensandbox_server.api.schema import Endpoint, Sandbox, SandboxFilter
from opensandbox_server.services.constants import OPEN_SANDBOX_INGRESS_HEADER
from opensandbox_server.config import (
    GATEWAY_ROUTE_MODE_HEADER,
    GATEWAY_ROUTE_MODE_URI,
    GATEWAY_ROUTE_MODE_WILDCARD,
    INGRESS_MODE_GATEWAY,
    IngressConfig,
)

logger = logging.getLogger(__name__)

MEMORY_PATTERN = re.compile(r"^\s*(\d+)([kmgti]i?|[kmgti]?b)?\s*$", re.IGNORECASE)
MEMORY_MULTIPLIERS: Dict[str, int] = {
    "": 1,
    "b": 1,
    "k": 1_000,
    "kb": 1_000,
    "ki": 1024,
    "m": 1_000_000,
    "mb": 1_000_000,
    "mi": 1024**2,
    "g": 1_000_000_000,
    "gb": 1_000_000_000,
    "gi": 1024**3,
    "t": 1_000_000_000_000,
    "tb": 1_000_000_000_000,
    "ti": 1024**4,
}


def parse_memory_limit(value: Optional[str]) -> Optional[int]:
    """Convert memory string (e.g., 512Mi) to bytes."""
    if not value:
        return None
    match = MEMORY_PATTERN.match(value)
    if not match:
        logger.warning("Invalid memory limit format '%s'; ignoring.", value)
        return None
    amount = int(match.group(1))
    unit = (match.group(2) or "").lower()
    multiplier = MEMORY_MULTIPLIERS.get(unit)
    if not multiplier:
        logger.warning("Unsupported memory unit '%s'; ignoring.", unit)
        return None
    return amount * multiplier


def parse_nano_cpus(value: Optional[str]) -> Optional[int]:
    """Convert CPU string (e.g., 500m, 2) to nano_cpus."""
    if not value:
        return None
    cpu_str = value.strip().lower()
    try:
        if cpu_str.endswith("m"):
            cpus = float(cpu_str[:-1]) / 1000
        else:
            cpus = float(cpu_str)
    except ValueError:
        logger.warning("Invalid CPU limit format '%s'; ignoring.", value)
        return None
    if cpus <= 0:
        logger.warning("CPU limit must be positive. Got '%s'. Ignoring.", value)
        return None
    return int(cpus * 1_000_000_000)


def parse_gpu_request(value: Optional[str]) -> Optional[int]:
    """Convert GPU limit string to a device count.

    Accepts a positive integer string (e.g. ``"1"``, ``"2"``) or the literal
    ``"all"`` to request every available GPU. Returns ``-1`` for ``"all"``
    (the sentinel the Docker Engine uses for unbounded device requests),
    a positive int for a numeric count, and ``None`` when unset or the
    value cannot be parsed.
    """
    if not value:
        return None
    gpu_str = value.strip().lower()
    if gpu_str == "all":
        return -1
    try:
        count = int(gpu_str)
    except ValueError:
        logger.warning("Invalid GPU limit format '%s'; ignoring.", value)
        return None
    if count <= 0:
        logger.warning("GPU limit must be positive. Got '%s'. Ignoring.", value)
        return None
    return count


def parse_timestamp(timestamp: Optional[str]) -> datetime:
    """
    Parse RFC3339 timestamp into timezone-aware datetime. Fallback to now.

    Docker often returns RFC3339Nano (up to 9 fractional digits). Python's
    datetime.fromisoformat only supports microseconds (6 digits), so we
    truncate the fractional part to 6 digits before parsing.
    """
    if not timestamp or timestamp == "0001-01-01T00:00:00Z":
        return datetime.now(timezone.utc)

    normalized = timestamp
    if normalized.endswith("Z"):
        normalized = normalized[:-1] + "+00:00"

    if "." in normalized:
        main, rest = normalized.split(".", 1)
        tz_sep = None
        for sep in ("+", "-"):
            pos = rest.find(sep)
            if pos != -1:
                tz_sep = pos
                break
        if tz_sep is None:
            frac = rest
            tz = ""
        else:
            frac = rest[:tz_sep]
            tz = rest[tz_sep:]
        frac = frac[:6]  # truncate to microseconds precision
        normalized = f"{main}.{frac}{tz}" if frac else f"{main}{tz}"

    try:
        return datetime.fromisoformat(normalized)
    except ValueError:
        logger.warning("Invalid timestamp '%s'; defaulting to current time.", timestamp)
        return datetime.now(timezone.utc)


def normalize_external_endpoint_url(endpoint: str, default_scheme: str = "https") -> str:
    """Normalize host or URL to a full URL with an explicit scheme."""
    endpoint = endpoint.strip()
    if endpoint.startswith("http://") or endpoint.startswith("https://"):
        return endpoint
    return f"{default_scheme}://{endpoint}"


def matches_filter(sandbox: Sandbox, filter_: SandboxFilter) -> bool:
    """Apply state/metadata filters to a sandbox instance."""
    if not filter_:
        return True
    if filter_.state:
        desired = {state.lower() for state in filter_.state}
        current_state = (sandbox.status.state or "").lower()
        if current_state not in desired:
            return False
    if filter_.metadata:
        metadata = sandbox.metadata or {}
        for key, value in filter_.metadata.items():
            if metadata.get(key) != value:
                return False
    return True


# ============================================================================
# Ingress helpers
# ============================================================================
def format_ingress_endpoint(
    ingress_config: Optional[IngressConfig],
    sandbox_id: str,
    port: int,
    expires_b36: Optional[str] = None,
    signature: Optional[str] = None,
) -> Optional[Endpoint]:
    """
    Build an ingress-based endpoint string for a sandbox.

    When *expires_b36* and *signature* are provided, the endpoint embeds a
    signed route token (OSEP-0011). Otherwise a plain ingress endpoint is returned.

    Returns None when ingress is not in gateway mode.
    """
    if not ingress_config or ingress_config.mode != INGRESS_MODE_GATEWAY:
        return None
    gateway_cfg = ingress_config.gateway
    if gateway_cfg is None:
        return None

    address = gateway_cfg.address
    route_mode = gateway_cfg.route.mode
    is_signed = expires_b36 is not None and signature is not None

    if route_mode == GATEWAY_ROUTE_MODE_WILDCARD:
        base = address[2:] if address.startswith("*.") else address
        if is_signed:
            return Endpoint(endpoint=f"{sandbox_id}-{port}-{expires_b36}-{signature}.{base}")
        return Endpoint(endpoint=f"{sandbox_id}-{port}.{base}")

    if route_mode == GATEWAY_ROUTE_MODE_URI:
        if is_signed:
            return Endpoint(endpoint=f"{address}/{sandbox_id}/{port}/{expires_b36}/{signature}")
        return Endpoint(endpoint=f"{address}/{sandbox_id}/{port}")

    if route_mode == GATEWAY_ROUTE_MODE_HEADER:
        if is_signed:
            header_value = f"{sandbox_id}-{port}-{expires_b36}-{signature}"
        else:
            header_value = f"{sandbox_id}-{port}"
        return Endpoint(
            endpoint=address,
            headers={OPEN_SANDBOX_INGRESS_HEADER: header_value},
        )

    raise RuntimeError(f"Unsupported route mode: {route_mode}")


__all__ = [
    "parse_memory_limit",
    "parse_nano_cpus",
    "parse_gpu_request",
    "parse_timestamp",
    "normalize_external_endpoint_url",
    "format_ingress_endpoint",
    "matches_filter",
]
