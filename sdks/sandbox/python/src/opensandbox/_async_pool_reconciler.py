#
# Copyright 2025 The OpenSandbox Authors
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
"""Async sandbox pool reconciliation logic."""

from __future__ import annotations

import logging
from collections.abc import Awaitable, Callable
from datetime import datetime, timezone

from opensandbox.pool_types import (
    AsyncPoolConfig,
    AsyncPoolStateStore,
)
from opensandbox.pool_types import (
    reap_expired_idle_with_min_ttl_async as _reap_expired_idle_with_min_ttl_async,
)

logger = logging.getLogger(__name__)


async def run_async_reconcile_tick(
    *,
    config: AsyncPoolConfig,
    state_store: AsyncPoolStateStore,
    on_discard_sandbox: Callable[[str], Awaitable[None]],
    warming_count: int,
    submit_warmups: Callable[[int], None],
    on_primary_acquired: Callable[[], None] = lambda: None,
) -> bool:
    pool_name = config.pool_name
    owner_id = str(config.owner_id)
    ttl = config.primary_lock_ttl

    if not await state_store.try_acquire_primary_lock(pool_name, owner_id, ttl):
        logger.debug(f"Async reconcile skip (not primary): pool_name={pool_name}")
        return False
    on_primary_acquired()
    await _run_primary_replenish_once(
        config=config,
        state_store=state_store,
        on_discard_sandbox=on_discard_sandbox,
        warming_count=warming_count,
        submit_warmups=submit_warmups,
    )
    return True


async def _run_primary_replenish_once(
    *,
    config: AsyncPoolConfig,
    state_store: AsyncPoolStateStore,
    on_discard_sandbox: Callable[[str], Awaitable[None]],
    warming_count: int,
    submit_warmups: Callable[[int], None],
) -> None:
    pool_name = config.pool_name
    owner_id = str(config.owner_id)
    ttl = config.primary_lock_ttl
    now = datetime.now(timezone.utc)

    discarded_alive = await _reap_expired_idle_with_min_ttl_async(
        state_store, pool_name, now, config.acquire_min_remaining_ttl
    )
    for sandbox_id in discarded_alive:
        await on_discard_sandbox(sandbox_id)
    counters = await state_store.snapshot_counters(pool_name)
    excess = max(0, counters.idle_count - config.max_idle)
    to_remove = min(excess, int(config.warmup_concurrency or 1))
    if to_remove > 0:
        await _shrink_excess_idle(config, state_store, on_discard_sandbox, to_remove)
        return

    deficit = max(0, config.max_idle - counters.idle_count - warming_count)
    to_create = min(deficit, config.warmup_create_qps)
    if to_create == 0:
        await state_store.renew_primary_lock(pool_name, owner_id, ttl)
        return

    if not await state_store.renew_primary_lock(pool_name, owner_id, ttl):
        return

    submit_warmups(to_create)


async def _shrink_excess_idle(
    config: AsyncPoolConfig,
    state_store: AsyncPoolStateStore,
    on_discard_sandbox: Callable[[str], Awaitable[None]],
    to_remove: int,
) -> None:
    pool_name = config.pool_name
    owner_id = str(config.owner_id)
    ttl = config.primary_lock_ttl
    removed = 0
    for _ in range(to_remove):
        if not await state_store.renew_primary_lock(pool_name, owner_id, ttl):
            logger.warning(
                f"Async reconcile lost primary lock before shrinking idle: pool_name={pool_name} removed={removed}"
            )
            return
        sandbox_id = await state_store.try_take_idle(pool_name)
        if sandbox_id is None:
            return
        await _discard(on_discard_sandbox, sandbox_id)
        removed += 1

    await state_store.renew_primary_lock(pool_name, owner_id, ttl)
    logger.debug(
        f"Async reconcile shrunk {removed} idle sandbox(es): pool_name={pool_name}"
    )


async def _discard(
    on_discard_sandbox: Callable[[str], Awaitable[None]], sandbox_id: str
) -> None:
    try:
        await on_discard_sandbox(sandbox_id)
    except Exception as exc:
        logger.warning(
            f"Async reconcile sandbox cleanup failed: sandbox_id={sandbox_id} error={exc}"
        )
