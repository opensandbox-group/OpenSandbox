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

"""redis_client tests: DSN sanitization and no client leak on failed PING."""

from unittest.mock import AsyncMock, MagicMock, patch

import pytest
from redis.exceptions import AuthenticationError

from opensandbox_server.integrations.idle_pause.redis_client import sanitize_dsn


def test_sanitize_dsn_strips_credentials():
    assert (
        sanitize_dsn("redis://:s3cret@redis.example:6379/2")
        == "redis://redis.example:6379/2"
    )
    assert (
        sanitize_dsn("redis://default:s3cret@10.0.0.1:6380")
        == "redis://10.0.0.1:6380"
    )
    assert sanitize_dsn("redis://127.0.0.1:6379/0") == "redis://127.0.0.1:6379/0"
    assert sanitize_dsn("  redis://h:1  ") == "redis://h:1"


def test_sanitize_dsn_unparsable_degrades_to_placeholder():
    # ":6379" makes urlsplit raise on the port access.
    assert sanitize_dsn("redis://host:notaport/0") == "<unparsable-redis-dsn>"


@pytest.mark.asyncio
async def test_connect_closes_client_when_ping_fails():
    from opensandbox_server.config import (
        AppConfig,
        AutomationConfig,
        IdlePauseConfig,
        IdlePauseRedisConfig,
        RuntimeConfig,
    )
    from opensandbox_server.integrations.idle_pause.redis_client import (
        connect_idle_pause_redis_from_config,
    )

    cfg = AppConfig(
        runtime=RuntimeConfig(type="kubernetes", execd_image="x"),
        automation=AutomationConfig(
            idle_pause=IdlePauseConfig(
                enabled=True,
                redis=IdlePauseRedisConfig(dsn="redis://127.0.0.1:6379/0"),
            )
        ),
    )
    client = MagicMock()
    client.ping = AsyncMock(side_effect=AuthenticationError("bad credentials"))
    client.aclose = AsyncMock()
    with patch(
        "opensandbox_server.integrations.idle_pause.redis_client.redis_async"
    ) as mock_module:
        mock_module.from_url.return_value = client
        with pytest.raises(AuthenticationError):
            await connect_idle_pause_redis_from_config(cfg)
    client.aclose.assert_awaited_once()
