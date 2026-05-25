#
# Copyright 2025 Alibaba Group Holding Ltd.
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

import asyncio
import logging
from collections.abc import Awaitable, Callable
from datetime import datetime, timezone

from opensandbox._pool_reconciler import ReconcileState
from opensandbox.pool_types import AsyncPoolConfig, AsyncPoolStateStore

logger = logging.getLogger(__name__)


async def run_async_reconcile_tick(
    *,
    config: AsyncPoolConfig,
    state_store: AsyncPoolStateStore,
    create_one: Callable[[], Awaitable[str | None]],
    on_discard_sandbox: Callable[[str], Awaitable[None]],
    reconcile_state: ReconcileState,
) -> None:
    pool_name = config.pool_name
    owner_id = str(config.owner_id)
    ttl = config.primary_lock_ttl

    if not await state_store.try_acquire_primary_lock(pool_name, owner_id, ttl):
        logger.debug("Async reconcile skip (not primary): pool_name=%s", pool_name)
        return
    await _run_primary_replenish_once(
        config=config,
        state_store=state_store,
        create_one=create_one,
        on_discard_sandbox=on_discard_sandbox,
        reconcile_state=reconcile_state,
    )


async def _run_primary_replenish_once(
    *,
    config: AsyncPoolConfig,
    state_store: AsyncPoolStateStore,
    create_one: Callable[[], Awaitable[str | None]],
    on_discard_sandbox: Callable[[str], Awaitable[None]],
    reconcile_state: ReconcileState,
) -> None:
    pool_name = config.pool_name
    owner_id = str(config.owner_id)
    ttl = config.primary_lock_ttl
    now = datetime.now(timezone.utc)

    await state_store.reap_expired_idle(pool_name, now)
    counters = await state_store.snapshot_counters(pool_name)
    excess = max(0, counters.idle_count - config.max_idle)
    to_remove = min(excess, int(config.warmup_concurrency or 1))
    if to_remove > 0:
        await _shrink_excess_idle(config, state_store, on_discard_sandbox, to_remove)
        return

    deficit = max(0, config.max_idle - counters.idle_count)
    to_create = min(deficit, int(config.warmup_concurrency or 1))
    if to_create == 0 or reconcile_state.is_backoff_active(now):
        await state_store.renew_primary_lock(pool_name, owner_id, ttl)
        return

    if not await state_store.renew_primary_lock(pool_name, owner_id, ttl):
        return

    results = await asyncio.gather(
        *(create_one() for _ in range(to_create)),
        return_exceptions=True,
    )
    created_ids: list[str] = []
    failure_count = 0
    last_error: str | None = None
    for result in results:
        if isinstance(result, BaseException):
            failure_count += 1
            last_error = str(result)
        elif result is not None:
            created_ids.append(result)
        else:
            failure_count += 1
            last_error = None
    reconcile_state.record_failures(failure_count, last_error)

    created = 0
    for index, sandbox_id in enumerate(created_ids):
        if not await state_store.renew_primary_lock(pool_name, owner_id, ttl):
            for orphaned_id in created_ids[index:]:
                await _discard(on_discard_sandbox, orphaned_id)
            logger.warning(
                "Async reconcile lost primary lock before put_idle; dropped %s newly created sandbox(es): pool_name=%s",
                len(created_ids) - index,
                pool_name,
            )
            return
        try:
            await state_store.put_idle(pool_name, sandbox_id)
            created += 1
            reconcile_state.record_success()
        except Exception as exc:
            reconcile_state.record_failure(str(exc))
            for orphaned_id in created_ids[index:]:
                try:
                    await state_store.remove_idle(pool_name, orphaned_id)
                except Exception:
                    pass
                await _discard(on_discard_sandbox, orphaned_id)
            logger.warning(
                "Async reconcile put_idle failed; dropped %s newly created sandbox(es): pool_name=%s error=%s",
                len(created_ids) - index,
                pool_name,
                exc,
            )
            return
    if created > 0:
        logger.debug("Async reconcile created %s sandboxes: pool_name=%s", created, pool_name)


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
                "Async reconcile lost primary lock before shrinking idle: pool_name=%s removed=%s",
                pool_name,
                removed,
            )
            return
        sandbox_id = await state_store.try_take_idle(pool_name)
        if sandbox_id is None:
            return
        await _discard(on_discard_sandbox, sandbox_id)
        removed += 1

    await state_store.renew_primary_lock(pool_name, owner_id, ttl)
    logger.debug("Async reconcile shrunk %s idle sandbox(es): pool_name=%s", removed, pool_name)


async def _discard(
    on_discard_sandbox: Callable[[str], Awaitable[None]], sandbox_id: str
) -> None:
    try:
        await on_discard_sandbox(sandbox_id)
    except Exception as exc:
        logger.warning(
            "Async reconcile sandbox cleanup failed: sandbox_id=%s error=%s",
            sandbox_id,
            exc,
        )
