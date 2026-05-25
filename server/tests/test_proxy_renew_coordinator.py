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

import asyncio
from unittest.mock import MagicMock

import pytest

from opensandbox_server.config import AppConfig, RenewIntentConfig, RuntimeConfig, ServerConfig
from opensandbox_server.integrations.renew_intent.consumer import RenewIntentConsumer, RenewWorkItem
from opensandbox_server.integrations.renew_intent.logutil import RENEW_SOURCE_SERVER_PROXY
from opensandbox_server.integrations.renew_intent.proxy_renew import ProxyRenewCoordinator


def _app_config(*, renew_enabled: bool = True, min_interval: int = 60) -> AppConfig:
    return AppConfig(
        server=ServerConfig(),
        renew_intent=RenewIntentConfig(
            enabled=renew_enabled,
            min_interval_seconds=min_interval,
        ),
        runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:latest"),
    )


def _consumer(cfg: AppConfig) -> RenewIntentConsumer:
    return RenewIntentConsumer(cfg, MagicMock(), MagicMock(), redis_client=None)


@pytest.mark.asyncio
async def test_proxy_schedule_noop_when_disabled(monkeypatch):
    cfg = _app_config(renew_enabled=False)
    coord = ProxyRenewCoordinator(cfg, _consumer(cfg))
    created: list[asyncio.Task[None]] = []

    def capture_task(coro, *, name=None):
        t = asyncio.get_event_loop().create_task(coro, name=name)
        created.append(t)
        return t

    monkeypatch.setattr(asyncio, "create_task", capture_task)
    coord.schedule("sbx-1")
    await asyncio.sleep(0)
    assert created == []


@pytest.mark.asyncio
async def test_proxy_schedule_noop_when_consumer_none():
    cfg = _app_config(renew_enabled=True)
    coord = ProxyRenewCoordinator(cfg, None)
    coord.schedule("sbx-1")


@pytest.mark.asyncio
async def test_proxy_min_interval_skips_second_attempt(monkeypatch):
    cfg = _app_config(renew_enabled=True, min_interval=60)
    consumer = _consumer(cfg)
    attempts = {"n": 0}

    def attempt(_sid: str, *, source: str) -> bool:
        attempts["n"] += 1
        return True

    consumer._controller.attempt_renew_sync = attempt  # type: ignore[method-assign]

    seq = iter([100.0, 100.0, 100.5])

    def mono():
        return next(seq, 999.0)

    monkeypatch.setattr(
        "opensandbox_server.integrations.renew_intent.consumer.time.monotonic",
        mono,
    )

    work = RenewWorkItem(
        source=RENEW_SOURCE_SERVER_PROXY,
        sandbox_id="sbx-1",
        observed_at=MagicMock(),
    )
    await consumer._process_work(work)
    await consumer._process_work(work)
    assert attempts["n"] == 1


@pytest.mark.asyncio
async def test_proxy_second_attempt_after_cooldown_window(monkeypatch):
    cfg = _app_config(renew_enabled=True, min_interval=60)
    consumer = _consumer(cfg)
    attempts = {"n": 0}

    def attempt(_sid: str, *, source: str) -> bool:
        attempts["n"] += 1
        return True

    consumer._controller.attempt_renew_sync = attempt  # type: ignore[method-assign]

    seq = iter([100.0, 100.0, 200.0, 200.0])

    def mono():
        return next(seq, 999.0)

    monkeypatch.setattr(
        "opensandbox_server.integrations.renew_intent.consumer.time.monotonic",
        mono,
    )

    work = RenewWorkItem(
        source=RENEW_SOURCE_SERVER_PROXY,
        sandbox_id="sbx-1",
        observed_at=MagicMock(),
    )
    await consumer._process_work(work)
    await consumer._process_work(work)
    assert attempts["n"] == 2


def test_consumer_mem_lru_drops_oldest_unlocked_entries():
    cfg = _app_config(renew_enabled=True)
    consumer = _consumer(cfg)
    consumer._max_tracked = 2

    consumer._ensure_mru_mem("a")
    consumer._ensure_mru_mem("b")
    assert set(consumer._mem_states) == {"a", "b"}

    consumer._ensure_mru_mem("c")
    assert set(consumer._mem_states) == {"b", "c"}
