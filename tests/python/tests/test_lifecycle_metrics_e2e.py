#
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
#
"""E2E: SDK create reports sandbox.create metrics to the lifecycle server."""

from __future__ import annotations

import asyncio
import logging
from datetime import timedelta
from unittest.mock import patch

import httpx
import pytest
from opensandbox import Sandbox
from opensandbox.internal import lifecycle_metrics
from opensandbox.models.sandboxes import SandboxImageSpec
from opensandbox.sync import SandboxSync

from tests.base_e2e_test import (
    TEST_API_KEY,
    TEST_DOMAIN,
    TEST_PROTOCOL,
    create_connection_config,
    create_connection_config_sync,
    get_e2e_sandbox_resource,
    get_sandbox_image,
)

logger = logging.getLogger(__name__)


def _metrics_url() -> str:
    return f"{TEST_PROTOCOL}://{TEST_DOMAIN}/v1/metrics/events"


async def _wait_until(predicate, *, timeout_s: float = 5.0, interval_s: float = 0.05):
    deadline = asyncio.get_running_loop().time() + timeout_s
    while asyncio.get_running_loop().time() < deadline:
        if predicate():
            return
        await asyncio.sleep(interval_s)
    raise AssertionError("timed out waiting for condition")


@pytest.mark.asyncio
async def test_server_accepts_metrics_event():
    """Direct lifecycle endpoint smoke (same path SDK uses)."""
    payload = {
        "eventType": "sandbox.create",
        "sandboxId": "e2e-metrics-direct",
        "image": get_sandbox_image(),
        "createDurationMs": 42,
        "sdkLanguage": "python",
        "sdkVersion": "e2e",
        "success": True,
    }
    async with httpx.AsyncClient(timeout=30.0) as client:
        response = await client.post(
            _metrics_url(),
            json=payload,
            headers={"OPEN-SANDBOX-API-KEY": TEST_API_KEY},
        )
    assert response.status_code == 204, response.text
    assert response.content == b""


@pytest.mark.asyncio
async def test_sandbox_create_reports_lifecycle_metrics():
    """Real create → SDK fire-and-forget POST /v1/metrics/events with expected body."""
    captured: list[dict] = []
    original = lifecycle_metrics._post_async

    async def _capture(config, payload):
        captured.append(dict(payload))
        await original(config, payload)

    connection_config = create_connection_config()
    sandbox = None
    with patch.object(lifecycle_metrics, "_post_async", side_effect=_capture):
        sandbox = await Sandbox.create(
            image=SandboxImageSpec(get_sandbox_image()),
            resource=get_e2e_sandbox_resource(),
            connection_config=connection_config,
            timeout=timedelta(minutes=5),
            ready_timeout=timedelta(seconds=60),
            entrypoint=["tail", "-f", "/dev/null"],
            env={"EXECD_API_GRACE_SHUTDOWN": "3s"},
            metadata={"tag": "e2e-lifecycle-metrics"},
        )
        try:
            await _wait_until(lambda: len(captured) >= 1)
        finally:
            if sandbox is not None:
                try:
                    await sandbox.kill()
                except Exception:
                    logger.warning("cleanup kill failed", exc_info=True)
                try:
                    await sandbox.close()
                except Exception:
                    logger.warning("cleanup close failed", exc_info=True)

    assert len(captured) >= 1
    event = captured[0]
    assert event["eventType"] == "sandbox.create"
    assert event["sdkLanguage"] == "python"
    assert event["success"] is True
    assert event["sandboxId"] == sandbox.id
    assert isinstance(event["sdkVersion"], str) and event["sdkVersion"]
    assert isinstance(event["createDurationMs"], int)
    assert event["createDurationMs"] > 0
    logger.info(
        "sandbox.create metrics createDurationMs=%s ms sandboxId=%s",
        event["createDurationMs"],
        event["sandboxId"],
    )
    assert event.get("image")


def test_sandbox_sync_create_reports_lifecycle_metrics():
    """Sync create also posts sandbox.create metrics."""
    captured: list[dict] = []
    original = lifecycle_metrics._post_sync

    def _capture(config, payload):
        captured.append(dict(payload))
        original(config, payload)

    connection_config = create_connection_config_sync()
    sandbox = None
    with patch.object(lifecycle_metrics, "_post_sync", side_effect=_capture):
        sandbox = SandboxSync.create(
            image=SandboxImageSpec(get_sandbox_image()),
            resource=get_e2e_sandbox_resource(),
            connection_config=connection_config,
            timeout=timedelta(minutes=5),
            ready_timeout=timedelta(seconds=60),
            entrypoint=["tail", "-f", "/dev/null"],
            env={"EXECD_API_GRACE_SHUTDOWN": "3s"},
            metadata={"tag": "e2e-lifecycle-metrics-sync"},
        )
        try:
            import time

            deadline = time.time() + 5.0
            while time.time() < deadline and not captured:
                time.sleep(0.05)
        finally:
            if sandbox is not None:
                try:
                    sandbox.kill()
                except Exception:
                    logger.warning("sync cleanup kill failed", exc_info=True)
                try:
                    sandbox.close()
                except Exception:
                    logger.warning("sync cleanup close failed", exc_info=True)

    assert len(captured) >= 1
    event = captured[0]
    assert event["eventType"] == "sandbox.create"
    assert event["sdkLanguage"] == "python"
    assert event["success"] is True
    assert event["sandboxId"] == sandbox.id
    assert event["createDurationMs"] > 0
    logger.info(
        "sandbox.create metrics (sync) createDurationMs=%s ms sandboxId=%s",
        event["createDurationMs"],
        event["sandboxId"],
    )
