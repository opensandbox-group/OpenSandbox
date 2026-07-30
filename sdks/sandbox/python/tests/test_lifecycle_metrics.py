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

"""Tests for best-effort lifecycle metrics reporting."""

import asyncio
from datetime import timedelta
from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest

from opensandbox.config import ConnectionConfig, ConnectionConfigSync
from opensandbox.internal.lifecycle_metrics import (
    _build_payload,
    _post_async,
    _post_sync,
    report_sandbox_create_metric,
)


def test_build_payload_omits_optional_fields():
    payload = _build_payload(
        sandbox_id=None,
        image=None,
        create_duration_ms=12,
        success=False,
    )
    assert payload["eventType"] == "sandbox.create"
    assert payload["createDurationMs"] == 12
    assert payload["success"] is False
    assert "sandboxId" not in payload
    assert "image" not in payload
    assert "sdkLanguage" not in payload
    assert "sdkVersion" not in payload


def test_sync_metrics_client_honors_redirect_config_and_security_hook():
    user_hook = MagicMock()
    config = ConnectionConfigSync(
        domain="sandbox.local:8080",
        protocol="http",
        follow_redirects=True,
        event_hooks={"request": [user_hook]},
    )
    client = MagicMock()
    context_manager = MagicMock()
    context_manager.__enter__.return_value = client

    with patch(
        "opensandbox.internal.lifecycle_metrics.httpx.Client",
        return_value=context_manager,
    ) as client_cls:
        _post_sync(config, {"eventType": "sandbox.create"})

    options = client_cls.call_args.kwargs
    assert options["follow_redirects"] is True
    assert "transport" not in options
    assert len(options["event_hooks"]["request"]) == 1

    redirected_request = httpx.Request(
        "POST",
        "http://redirect.local/metrics/events",
        headers={
            "OPEN-SANDBOX-API-KEY": "api-secret",
            "OPENSANDBOX-EGRESS-AUTH": "egress-secret",
            "OpenSandbox-Secure-Access": "access-secret",
            "X-Custom-Header": "preserved",
        },
    )
    options["event_hooks"]["request"][0](redirected_request)

    assert user_hook.call_count == 0
    assert "OPEN-SANDBOX-API-KEY" not in redirected_request.headers
    assert "OPENSANDBOX-EGRESS-AUTH" not in redirected_request.headers
    assert "OpenSandbox-Secure-Access" not in redirected_request.headers
    assert redirected_request.headers["X-Custom-Header"] == "preserved"
    client.post.assert_called_once()


@pytest.mark.asyncio
async def test_async_metrics_client_honors_redirect_config_and_security_hook():
    user_hook = AsyncMock()
    config = ConnectionConfig(
        domain="sandbox.local:8080",
        protocol="http",
        follow_redirects=True,
        event_hooks={"request": [user_hook]},
    )
    client = MagicMock()
    client.post = AsyncMock()
    context_manager = MagicMock()
    context_manager.__aenter__ = AsyncMock(return_value=client)
    context_manager.__aexit__ = AsyncMock(return_value=None)

    with patch(
        "opensandbox.internal.lifecycle_metrics.httpx.AsyncClient",
        return_value=context_manager,
    ) as client_cls:
        await _post_async(config, {"eventType": "sandbox.create"})

    options = client_cls.call_args.kwargs
    assert options["follow_redirects"] is True
    assert "transport" not in options
    assert len(options["event_hooks"]["request"]) == 1

    redirected_request = httpx.Request(
        "POST",
        "http://redirect.local/metrics/events",
        headers={
            "OPEN-SANDBOX-API-KEY": "api-secret",
            "OPENSANDBOX-EGRESS-AUTH": "egress-secret",
            "OpenSandbox-Secure-Access": "access-secret",
            "X-Custom-Header": "preserved",
        },
    )
    await options["event_hooks"]["request"][0](redirected_request)

    assert user_hook.await_count == 0
    assert "OPEN-SANDBOX-API-KEY" not in redirected_request.headers
    assert "OPENSANDBOX-EGRESS-AUTH" not in redirected_request.headers
    assert "OpenSandbox-Secure-Access" not in redirected_request.headers
    assert redirected_request.headers["X-Custom-Header"] == "preserved"
    client.post.assert_awaited_once()


@pytest.mark.asyncio
async def test_report_does_not_raise_when_post_fails():
    config = ConnectionConfig(
        domain="127.0.0.1:9",
        protocol="http",
        request_timeout=timedelta(milliseconds=50),
    )

    with patch(
        "opensandbox.internal.lifecycle_metrics._post_async",
        side_effect=httpx.ConnectError("boom"),
    ):
        report_sandbox_create_metric(
            config,
            sandbox_id="sbx",
            image="python:3.12",
            create_duration_ms=100,
            success=True,
        )
        # Allow the scheduled task to run and finish.
        await asyncio.sleep(0.05)


def test_sync_report_swallows_errors_in_thread():
    config = ConnectionConfig(
        domain="127.0.0.1:9",
        protocol="http",
        request_timeout=timedelta(milliseconds=50),
    )
    done = MagicMock()

    def _boom(*_args, **_kwargs):
        done()
        raise httpx.ConnectError("boom")

    with patch(
        "opensandbox.internal.lifecycle_metrics._post_sync",
        side_effect=_boom,
    ):
        # No running loop → thread path.
        report_sandbox_create_metric(
            config,
            sandbox_id=None,
            image="img",
            create_duration_ms=1,
            success=False,
        )
        import time

        for _ in range(50):
            if done.called:
                break
            time.sleep(0.02)
    assert done.called


@pytest.mark.asyncio
async def test_report_skipped_when_disable_metrics():
    config = ConnectionConfig(
        domain="127.0.0.1:9",
        protocol="http",
        disable_metrics=True,
    )
    with patch("opensandbox.internal.lifecycle_metrics._post_async") as post:
        report_sandbox_create_metric(
            config,
            sandbox_id="sbx",
            image="python:3.12",
            create_duration_ms=100,
            success=True,
        )
        await asyncio.sleep(0.05)
    post.assert_not_called()


def test_report_skipped_when_env_disables_metrics(monkeypatch):
    monkeypatch.setenv("OPENSANDBOX_DISABLE_METRICS", "1")
    config = ConnectionConfig(domain="127.0.0.1:9", protocol="http")
    with patch("opensandbox.internal.lifecycle_metrics._post_sync") as post:
        report_sandbox_create_metric(
            config,
            sandbox_id=None,
            image="img",
            create_duration_ms=1,
            success=False,
        )
        import time

        time.sleep(0.05)
    post.assert_not_called()


def test_report_never_raises_when_construction_fails_sync():
    # Regression test: previously, payload construction and event-loop /
    # thread scheduling ran outside any try/except. If they raised, the
    # telemetry exception would replace the original Sandbox.create failure
    # when the reporter is called from the create catch path.
    config = ConnectionConfig(
        domain="127.0.0.1:9",
        protocol="http",
        request_timeout=timedelta(milliseconds=50),
    )

    with patch(
        "opensandbox.internal.lifecycle_metrics._build_payload",
        side_effect=RuntimeError("boom in construction"),
    ):
        # No running loop → sync/thread scheduling path. Must not raise.
        report_sandbox_create_metric(
            config,
            sandbox_id="sbx",
            image="python:3.12",
            create_duration_ms=100,
            success=False,
        )


@pytest.mark.asyncio
async def test_report_never_raises_when_construction_fails_async():
    # Same regression, running on an active event loop → task scheduling path.
    config = ConnectionConfig(
        domain="127.0.0.1:9",
        protocol="http",
        request_timeout=timedelta(milliseconds=50),
    )

    with patch(
        "opensandbox.internal.lifecycle_metrics._build_payload",
        side_effect=RuntimeError("boom in construction"),
    ):
        # Must not raise even though _build_payload throws before scheduling.
        report_sandbox_create_metric(
            config,
            sandbox_id="sbx",
            image="python:3.12",
            create_duration_ms=100,
            success=False,
        )


@pytest.mark.asyncio
async def test_async_report_keeps_strong_task_ref():
    config = ConnectionConfig(
        domain="127.0.0.1:9",
        protocol="http",
        request_timeout=timedelta(milliseconds=50),
    )
    from opensandbox.internal import lifecycle_metrics as metrics_mod

    started = asyncio.Event()

    async def _slow_post(*_args, **_kwargs):
        started.set()
        await asyncio.sleep(0.05)

    with patch.object(metrics_mod, "_post_async", side_effect=_slow_post):
        report_sandbox_create_metric(
            config,
            sandbox_id="sbx",
            image="img",
            create_duration_ms=10,
            success=True,
        )
        assert len(metrics_mod._pending) == 1
        await started.wait()
        await asyncio.sleep(0.08)
    assert len(metrics_mod._pending) == 0
