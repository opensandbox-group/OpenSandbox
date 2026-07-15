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

"""Structured ``renew_*`` keys for ``logging`` ``extra`` and message suffix lines."""

from __future__ import annotations

from typing import Any

RENEW_SOURCE_SERVER_PROXY = "server_proxy"
RENEW_SOURCE_REDIS_QUEUE = "redis_queue"

RENEW_EVENT_SUCCEEDED = "renew_succeeded"
RENEW_EVENT_FAILED = "renew_failed"
RENEW_EVENT_TASK_FAILED = "renew_task_failed"
RENEW_EVENT_WORKERS_STARTED = "workers_started"
RENEW_EVENT_WORKERS_NOT_STARTED = "workers_not_started"
RENEW_EVENT_REDIS_CONNECTED = "redis_connected"


def _renew_extra(
    *,
    event: str,
    source: str,
    sandbox_id: str | None = None,
    skip_reason: str | None = None,
    **fields: Any,
) -> dict[str, Any]:
    out: dict[str, Any] = {
        "renew_event": event,
        "renew_source": source,
    }
    if sandbox_id is not None:
        out["renew_sandbox_id"] = sandbox_id
    if skip_reason is not None:
        out["renew_skip_reason"] = skip_reason
    for k, v in fields.items():
        if v is not None:
            key = k if k.startswith("renew_") else f"renew_{k}"
            out[key] = v
    return out


def renew_bundle(**kwargs: Any) -> tuple[str, dict[str, Any]]:
    """``(k=v line, extra dict)`` for one log call."""
    extra = _renew_extra(**kwargs)
    line = " ".join(f"{k}={extra[k]!s}" for k in sorted(extra.keys()))
    return line, extra
