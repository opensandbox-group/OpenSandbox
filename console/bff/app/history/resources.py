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

from __future__ import annotations

import re
from typing import Any

_DEFAULT_CPU = "1"
_DEFAULT_MEMORY = "512Mi"

_MEMORY_RE = re.compile(
    r"^(\d+(?:\.\d+)?)\s*(Ki|Mi|Gi|Ti|K|M|G|T)?$",
    re.IGNORECASE,
)


def explicit_resource_limits(
    sandbox: dict[str, Any],
    create_request: dict[str, Any] | None,
) -> dict[str, str] | None:
    """Limits from Lifecycle sandbox or stored create body only; no usage defaults."""
    for source in (sandbox, create_request or {}):
        rl = source.get("resourceLimits")
        if isinstance(rl, dict) and rl:
            out: dict[str, str] = {}
            for k, v in rl.items():
                if v is not None:
                    out[str(k)] = str(v)
            if out:
                return out
    return None


def resource_limits_dict(
    sandbox: dict[str, Any],
    create_request: dict[str, Any] | None,
) -> dict[str, str]:
    explicit = explicit_resource_limits(sandbox, create_request)
    if explicit:
        return explicit
    return {"cpu": _DEFAULT_CPU, "memory": _DEFAULT_MEMORY}


def parse_cpu_cores(value: str | None) -> float:
    if not value:
        return 1.0
    raw = value.strip()
    if raw.endswith("m"):
        try:
            return float(raw[:-1]) / 1000.0
        except ValueError:
            return 1.0
    try:
        return float(raw)
    except ValueError:
        return 1.0


def parse_memory_gi(value: str | None) -> float:
    if not value:
        return 0.5
    raw = value.strip()
    m = _MEMORY_RE.match(raw)
    if not m:
        try:
            return float(raw) / (1024**3)
        except ValueError:
            return 0.5
    num = float(m.group(1))
    unit = (m.group(2) or "").upper()
    if unit in ("", "B"):
        return num / (1024**3)
    if unit in ("K", "KI"):
        return num / (1024**2)
    if unit in ("M", "MI"):
        return num / 1024
    if unit in ("G", "GI"):
        return num
    if unit in ("T", "TI"):
        return num * 1024
    return num / 1024


def limits_to_numbers(limits: dict[str, str]) -> tuple[str | None, str | None, float, float]:
    cpu_s = limits.get("cpu")
    mem_s = limits.get("memory")
    return cpu_s, mem_s, parse_cpu_cores(cpu_s), parse_memory_gi(mem_s)
