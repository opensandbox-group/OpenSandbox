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

from unittest.mock import AsyncMock, patch

import pytest

from opensandbox_server.config import AppConfig, RenewIntentConfig, RenewIntentRedisConfig, RuntimeConfig, ServerConfig
from opensandbox_server.integrations.renew_intent.redis_client import (
    close_renew_intent_redis_client,
    connect_renew_intent_redis_from_config,
)


def _minimal_app_config(roa: RenewIntentConfig) -> AppConfig:
    return AppConfig(
        server=ServerConfig(),
        runtime=RuntimeConfig(type="docker", execd_image="execd:latest"),
        renew_intent=roa,
    )


@pytest.mark.asyncio
async def test_connect_returns_none_when_renew_intent_disabled():
    roa = RenewIntentConfig(
        enabled=False,
        redis=RenewIntentRedisConfig(enabled=True, dsn="redis://x"),
    )
    cfg = _minimal_app_config(roa)
    assert await connect_renew_intent_redis_from_config(cfg) is None


@pytest.mark.asyncio
async def test_connect_returns_none_when_redis_disabled():
    roa = RenewIntentConfig(
        enabled=True,
        redis=RenewIntentRedisConfig(enabled=False),
    )
    cfg = _minimal_app_config(roa)
    assert await connect_renew_intent_redis_from_config(cfg) is None


@pytest.mark.asyncio
@patch("opensandbox_server.integrations.renew_intent.redis_client.redis_async")
async def test_connect_pings_when_enabled(mock_redis_mod):
    mock_client = AsyncMock()
    mock_redis_mod.from_url.return_value = mock_client
    roa = RenewIntentConfig(
        enabled=True,
        redis=RenewIntentRedisConfig(
            enabled=True,
            dsn="redis://127.0.0.1:6379/0",
            queue_key="q",
            consumer_concurrency=2,
        ),
    )
    cfg = _minimal_app_config(roa)

    client = await connect_renew_intent_redis_from_config(cfg)

    assert client is mock_client
    mock_redis_mod.from_url.assert_called_once()
    call_kw = mock_redis_mod.from_url.call_args
    assert call_kw[0][0] == "redis://127.0.0.1:6379/0"
    assert call_kw[1].get("decode_responses") is True
    mock_client.ping.assert_awaited_once()


@pytest.mark.asyncio
async def test_close_none_is_safe():
    await close_renew_intent_redis_client(None)


@pytest.mark.asyncio
async def test_close_client():
    client = AsyncMock()
    await close_renew_intent_redis_client(client)
    client.aclose.assert_awaited_once()
