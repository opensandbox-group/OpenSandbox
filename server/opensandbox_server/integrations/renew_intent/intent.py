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

"""Renew-intent JSON (matches components/ingress/pkg/renewintent Intent)."""

from __future__ import annotations

import json
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any, Optional


@dataclass(frozen=True)
class RenewIntent:
    sandbox_id: str
    observed_at: datetime
    port: Optional[int] = None
    request_uri: Optional[str] = None


def _parse_rfc3339_time(value: str) -> Optional[datetime]:
    s = value.strip()
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    # Ingress uses Go RFC3339Nano (up to 9 fractional digits); CPython fromisoformat allows at most 6.
    dot = s.find(".")
    if dot != -1:
        end = dot + 1
        while end < len(s) and s[end].isdigit():
            end += 1
        frac = s[dot + 1 : end]
        if len(frac) > 6:
            s = s[: dot + 1] + frac[:6] + s[end:]
    try:
        dt = datetime.fromisoformat(s)
    except ValueError:
        return None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt


def parse_renew_intent_json(raw: str) -> Optional[RenewIntent]:
    """Parse ingress LPUSH JSON payload; return ``None`` if invalid."""
    try:
        data: dict[str, Any] = json.loads(raw)
    except (json.JSONDecodeError, TypeError):
        return None
    sid = data.get("sandbox_id")
    if not isinstance(sid, str) or not sid.strip():
        return None
    obs_raw = data.get("observed_at")
    if not isinstance(obs_raw, str) or not obs_raw.strip():
        return None
    observed_at = _parse_rfc3339_time(obs_raw)
    if observed_at is None:
        return None

    port: Optional[int] = None
    if "port" in data and data["port"] is not None:
        try:
            port = int(data["port"])
        except (TypeError, ValueError):
            port = None

    uri = data.get("request_uri")
    request_uri = uri if isinstance(uri, str) else None

    return RenewIntent(
        sandbox_id=sid.strip(),
        observed_at=observed_at,
        port=port,
        request_uri=request_uri,
    )
