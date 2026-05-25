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

"""Async Redis client for renew-intent queue consumers."""

from __future__ import annotations

import logging
from typing import Optional

import redis.asyncio as redis_async
from redis.asyncio import Redis

from opensandbox_server.config import AppConfig
from opensandbox_server.integrations.renew_intent.logutil import (
    RENEW_EVENT_REDIS_CONNECTED,
    RENEW_SOURCE_REDIS_QUEUE,
    renew_bundle,
)

logger = logging.getLogger(__name__)


async def connect_renew_intent_redis_from_config(
    app_config: AppConfig,
) -> Optional[Redis]:
    """Connect (with ``PING``) or ``None`` if renew-intent Redis is disabled."""
    ri = app_config.renew_intent
    if not ri.enabled or not ri.redis.enabled:
        return None

    dsn = ri.redis.dsn
    if dsn is None or not str(dsn).strip():
        return None

    client = redis_async.from_url(
        str(dsn).strip(),
        decode_responses=True,
    )
    await client.ping()
    line, ex = renew_bundle(
        event=RENEW_EVENT_REDIS_CONNECTED,
        source=RENEW_SOURCE_REDIS_QUEUE,
        queue_key=ri.redis.queue_key,
        consumer_concurrency=ri.redis.consumer_concurrency,
    )
    logger.info(f"renew_intent {line}", extra=ex)
    return client


async def close_renew_intent_redis_client(client: Optional[Redis]) -> None:
    """Close client; no-op for ``None``."""
    if client is None:
        return
    await client.aclose()
