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

"""Async Redis client for the idle-pause integration (OSEP-0024)."""

from __future__ import annotations

import logging
from typing import Optional
from urllib.parse import urlsplit

import redis.asyncio as redis_async
from redis.asyncio import Redis

from opensandbox_server.config import AppConfig

logger = logging.getLogger(__name__)


def sanitize_dsn(dsn: str) -> str:
    """Strip credentials from a DSN for logging; unparsable -> placeholder."""
    try:
        parts = urlsplit(dsn.strip())
        port = f":{parts.port}" if parts.port else ""
        return f"{parts.scheme}://{parts.hostname}{port}{parts.path or ''}"
    except ValueError:
        return "<unparsable-redis-dsn>"


async def connect_idle_pause_redis_from_config(
    app_config: AppConfig,
) -> Optional[Redis]:
    """Connect (with PING) or ``None`` when disabled; closes on failed PING.

    Raises RedisError/OSError/ValueError — the caller degrades to
    "sweeper not started".
    """
    ip = app_config.automation.idle_pause
    if not ip.enabled:
        return None

    dsn = ip.redis.dsn
    if dsn is None or not str(dsn).strip():
        return None

    client = redis_async.from_url(str(dsn).strip(), decode_responses=True)
    try:
        await client.ping()
    except BaseException:
        await client.aclose()
        raise
    logger.info(
        f"idle_pause redis connected dsn={sanitize_dsn(str(dsn))} "
        f"lock_ttl={ip.redis.lock_ttl_seconds}"
    )
    return client


async def close_idle_pause_redis_client(client: Optional[Redis]) -> None:
    """Close client; no-op for ``None``."""
    if client is None:
        return
    await client.aclose()
