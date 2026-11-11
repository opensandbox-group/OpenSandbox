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

"""Pure idle decision over the activity key and its tombstone.

A missing activity key is *unknown*, never proof of idleness: it may delay
a pause (tombstone window), never accelerate one. Timestamps are Unix ms.
"""

from __future__ import annotations

import enum
from typing import NamedTuple, Optional


class IdleVerdict(enum.Enum):
    NOT_IDLE = "not_idle"  # activity younger than X
    IDLE = "idle"  # idle >= X (stale value, or tombstone aged >= X)
    UNKNOWN = "unknown"  # missing key / fresh tombstone: skip, never pause


def is_idle_by_value(last_active_ms: int, now_ms: int, threshold_seconds: int) -> bool:
    return now_ms - last_active_ms >= threshold_seconds * 1000


def is_idle_by_tombstone(tombstone_ms: int, now_ms: int, threshold_seconds: int) -> bool:
    """Inactivity measured from the first *observation* of unknown state."""
    return now_ms - tombstone_ms >= threshold_seconds * 1000


class IdleDecision(NamedTuple):
    verdict: IdleVerdict
    # Activity key was missing: create the tombstone before skipping.
    create_tombstone: bool


def evaluate_idle(
    *,
    last_active_ms: Optional[int],
    tombstone_ms: Optional[int],
    now_ms: int,
    threshold_seconds: int,
) -> IdleDecision:
    if last_active_ms is not None:
        verdict = (
            IdleVerdict.IDLE
            if is_idle_by_value(last_active_ms, now_ms, threshold_seconds)
            else IdleVerdict.NOT_IDLE
        )
        return IdleDecision(verdict=verdict, create_tombstone=False)
    if tombstone_ms is not None:
        verdict = (
            IdleVerdict.IDLE
            if is_idle_by_tombstone(tombstone_ms, now_ms, threshold_seconds)
            else IdleVerdict.UNKNOWN
        )
        return IdleDecision(verdict=verdict, create_tombstone=False)
    return IdleDecision(verdict=IdleVerdict.UNKNOWN, create_tombstone=True)


__all__ = [
    "IdleDecision",
    "IdleVerdict",
    "evaluate_idle",
    "is_idle_by_tombstone",
    "is_idle_by_value",
]
