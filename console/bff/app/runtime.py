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

from datetime import datetime, timezone
from typing import Any

_TERMINAL_STATES = frozenset({"Terminated", "Failed", "Stopping"})


def _parse_dt(value: str | None) -> datetime | None:
    if not value:
        return None
    # Lifecycle API uses ISO-8601; handle Z suffix.
    normalized = value.replace("Z", "+00:00")
    try:
        dt = datetime.fromisoformat(normalized)
    except ValueError:
        return None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc)


def attach_runtime_summary(sandbox: dict[str, Any], now: datetime | None = None) -> dict[str, Any]:
    """Return a shallow copy of sandbox with runtimeSummary added."""
    now = now or datetime.now(timezone.utc)
    created = _parse_dt(sandbox.get("createdAt"))
    expires = _parse_dt(sandbox.get("expiresAt"))
    status = sandbox.get("status") or {}
    state = status.get("state")
    last_transition = _parse_dt(status.get("lastTransitionAt"))

    summary: dict[str, Any] = {"asOf": now.isoformat().replace("+00:00", "Z"), "basis": "createdAt"}
    if created:
        end = now
        if state in _TERMINAL_STATES and last_transition:
            end = last_transition
            summary["basis"] = "lastTransitionAt"
        summary["wallClockSeconds"] = max(0, int((end - created).total_seconds()))
    if expires:
        summary["remainingSeconds"] = max(0, int((expires - now).total_seconds()))

    out = dict(sandbox)
    out["runtimeSummary"] = summary
    return out


def aggregate_runtime_stats(
    sandboxes: list[dict[str, Any]],
    now: datetime | None = None,
) -> dict[str, Any]:
    """Summarize already-attached runtime rows with one timestamp.

    Missing wall clocks are skipped. Terminal sandboxes are not treated as
    about to expire just because their remaining time is small.
    """
    if now is None:
        stamped: datetime | None = None
        for sandbox in sandboxes:
            raw = (sandbox.get("runtimeSummary") or {}).get("asOf")
            if isinstance(raw, str):
                stamped = _parse_dt(raw)
                if stamped is not None:
                    break
        now = stamped or datetime.now(timezone.utc)

    running = [s for s in sandboxes if (s.get("status") or {}).get("state") == "Running"]
    walls: list[int] = []
    expiring = 0
    for sandbox in sandboxes:
        summary = sandbox.get("runtimeSummary") or {}
        wall = summary.get("wallClockSeconds")
        if isinstance(wall, int):
            walls.append(wall)
        state = (sandbox.get("status") or {}).get("state")
        remaining = summary.get("remainingSeconds")
        if state not in _TERMINAL_STATES and isinstance(remaining, int) and remaining <= 30 * 60:
            expiring += 1

    total = sum(walls)
    return {
        "runningCount": len(running),
        "totalWallClockSeconds": total,
        "avgWallClockSeconds": int(total / len(walls)) if walls else 0,
        "maxWallClockSeconds": max(walls) if walls else 0,
        "expiringWithin30mCount": expiring,
        "asOf": now.isoformat().replace("+00:00", "Z"),
    }
