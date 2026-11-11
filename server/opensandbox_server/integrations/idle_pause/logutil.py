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

"""Structured ``idlep_*`` log keys, mirroring ``renew_intent/logutil.py``."""

from __future__ import annotations

from typing import Any

IDLEP_SOURCE_SWEEPER = "sweeper"

IDLEP_EVENT_SWEEP_SKIPPED = "sweep_skipped"
IDLEP_EVENT_SWEEP_COMPLETED = "sweep_completed"
IDLEP_EVENT_PAUSE_ISSUED = "pause_issued"
IDLEP_EVENT_TOMBSTONED = "unknown_tombstoned"


def _extra(
    *,
    event: str,
    source: str,
    sandbox_id: str | None = None,
    namespace: str | None = None,
    skip_reason: str | None = None,
    **fields: Any,
) -> dict[str, Any]:
    out: dict[str, Any] = {
        "idlep_event": event,
        "idlep_source": source,
    }
    if sandbox_id is not None:
        out["idlep_sandbox_id"] = sandbox_id
    if namespace is not None:
        out["idlep_namespace"] = namespace
    if skip_reason is not None:
        out["idlep_skip_reason"] = skip_reason
    for k, v in fields.items():
        if v is not None:
            key = k if k.startswith("idlep_") else f"idlep_{k}"
            out[key] = v
    return out


def idlep_bundle(**kwargs: Any) -> tuple[str, dict[str, Any]]:
    """``(k=v line, extra dict)`` for one log call."""
    extra = _extra(**kwargs)
    line = " ".join(f"{k}={extra[k]!s}" for k in sorted(extra.keys()))
    return line, extra
