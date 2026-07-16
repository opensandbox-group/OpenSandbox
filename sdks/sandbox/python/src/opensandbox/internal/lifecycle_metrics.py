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

"""Best-effort SDK metrics reporting to the lifecycle server."""

from __future__ import annotations

import asyncio
import logging
import os
import threading
from datetime import timedelta
from typing import Any, Protocol

import httpx

logger = logging.getLogger(__name__)

_DISABLE_METRICS_ENV = "OPENSANDBOX_DISABLE_METRICS"

# Keep strong refs so fire-and-forget tasks are not GC'd mid-flight.
_pending: set[asyncio.Task[Any]] = set()


class _MetricsConnection(Protocol):
    headers: dict[str, str]
    user_agent: str
    disable_metrics: bool

    def get_api_key(self) -> str: ...

    def get_base_url(self) -> str: ...

    @property
    def request_timeout(self) -> timedelta | float | int: ...


def _env_metrics_disabled() -> bool:
    return os.getenv(_DISABLE_METRICS_ENV, "").strip() == "1"


def _metrics_disabled(config: _MetricsConnection) -> bool:
    return bool(config.disable_metrics) or _env_metrics_disabled()


def _build_payload(
    *,
    sandbox_id: str | None,
    image: str | None,
    create_duration_ms: int,
    success: bool,
) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "eventType": "sandbox.create",
        "createDurationMs": max(0, int(create_duration_ms)),
        "success": bool(success),
    }
    if sandbox_id:
        payload["sandboxId"] = sandbox_id
    if image:
        payload["image"] = image
    return payload


def _headers(config: _MetricsConnection) -> dict[str, str]:
    headers = {"Content-Type": "application/json", **(config.headers or {})}
    api_key = config.get_api_key()
    if api_key and "OPEN-SANDBOX-API-KEY" not in headers:
        headers["OPEN-SANDBOX-API-KEY"] = api_key
    if config.user_agent and "User-Agent" not in headers and "user-agent" not in headers:
        headers["User-Agent"] = config.user_agent
    return headers


def _timeout_seconds(config: _MetricsConnection) -> float:
    timeout = config.request_timeout
    if isinstance(timeout, timedelta):
        return timeout.total_seconds()
    return float(timeout)


def _post_sync(config: _MetricsConnection, payload: dict[str, Any]) -> None:
    url = f"{config.get_base_url().rstrip('/')}/metrics/events"
    with httpx.Client(timeout=_timeout_seconds(config)) as client:
        client.post(url, json=payload, headers=_headers(config))


async def _post_async(config: _MetricsConnection, payload: dict[str, Any]) -> None:
    url = f"{config.get_base_url().rstrip('/')}/metrics/events"
    async with httpx.AsyncClient(timeout=_timeout_seconds(config)) as client:
        await client.post(url, json=payload, headers=_headers(config))


def report_sandbox_create_metric(
    config: _MetricsConnection,
    *,
    sandbox_id: str | None,
    image: str | None,
    create_duration_ms: int,
    success: bool,
) -> None:
    """Fire-and-forget create latency report. Never raises or blocks callers."""
    if _metrics_disabled(config):
        return

    payload = _build_payload(
        sandbox_id=sandbox_id,
        image=image,
        create_duration_ms=create_duration_ms,
        success=success,
    )

    try:
        loop = asyncio.get_running_loop()
    except RuntimeError:
        def _thread_run() -> None:
            try:
                _post_sync(config, payload)
            except Exception:
                logger.debug("Failed to report sandbox.create metrics", exc_info=True)

        threading.Thread(target=_thread_run, daemon=True).start()
        return

    async def _run() -> None:
        try:
            await _post_async(config, payload)
        except Exception:
            logger.debug("Failed to report sandbox.create metrics", exc_info=True)

    task = loop.create_task(_run())
    _pending.add(task)
    task.add_done_callback(_pending.discard)
