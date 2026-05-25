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
"""Sandbox pool reconciliation logic."""

from __future__ import annotations

import logging
from collections.abc import Callable
from concurrent.futures import Executor, wait
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone

from opensandbox.pool_types import PoolConfig, PoolState, PoolStateStore

logger = logging.getLogger(__name__)


@dataclass
class ReconcileState:
    degraded_threshold: int
    backoff_base: timedelta = timedelta(seconds=30)
    backoff_max: timedelta = timedelta(days=1)
    failure_count: int = 0
    state: PoolState = PoolState.HEALTHY
    last_error: str | None = None
    backoff_until: datetime | None = None
    backoff_attempts: int = 0

    def record_success(self) -> None:
        self.failure_count = 0
        if self.state == PoolState.DEGRADED:
            self.state = PoolState.HEALTHY
        self.backoff_until = None
        self.backoff_attempts = 0
        self.last_error = None

    def record_failure(self, error_message: str | None) -> None:
        self.record_failures(1, error_message)

    def record_failures(self, count: int, error_message: str | None) -> None:
        if count <= 0:
            return
        self.failure_count += count
        self.last_error = error_message
        if self.failure_count >= self.degraded_threshold:
            self.state = PoolState.DEGRADED
            self.backoff_attempts += 1
            exponent = min(self.backoff_attempts - 1, 30)
            delay = min(
                self.backoff_base.total_seconds() * (1 << exponent),
                self.backoff_max.total_seconds(),
            )
            self.backoff_until = datetime.now(timezone.utc) + timedelta(seconds=delay)

    def is_backoff_active(self, now: datetime | None = None) -> bool:
        until = self.backoff_until
        if until is None:
            return False
        return self.state == PoolState.DEGRADED and (now or datetime.now(timezone.utc)) < until


def run_reconcile_tick(
    *,
    config: PoolConfig,
    state_store: PoolStateStore,
    create_one: Callable[[], str | None],
    on_discard_sandbox: Callable[[str], None],
    reconcile_state: ReconcileState,
    warmup_executor: Executor,
) -> None:
    pool_name = config.pool_name
    owner_id = str(config.owner_id)
    ttl = config.primary_lock_ttl

    if not state_store.try_acquire_primary_lock(pool_name, owner_id, ttl):
        logger.debug("Reconcile skip (not primary): pool_name=%s", pool_name)
        return
    _run_primary_replenish_once(
        config=config,
        state_store=state_store,
        create_one=create_one,
        on_discard_sandbox=on_discard_sandbox,
        reconcile_state=reconcile_state,
        warmup_executor=warmup_executor,
    )


def _run_primary_replenish_once(
    *,
    config: PoolConfig,
    state_store: PoolStateStore,
    create_one: Callable[[], str | None],
    on_discard_sandbox: Callable[[str], None],
    reconcile_state: ReconcileState,
    warmup_executor: Executor,
) -> None:
    pool_name = config.pool_name
    owner_id = str(config.owner_id)
    ttl = config.primary_lock_ttl
    now = datetime.now(timezone.utc)

    state_store.reap_expired_idle(pool_name, now)
    counters = state_store.snapshot_counters(pool_name)
    excess = max(0, counters.idle_count - config.max_idle)
    to_remove = min(excess, int(config.warmup_concurrency or 1))
    if to_remove > 0:
        _shrink_excess_idle(config, state_store, on_discard_sandbox, to_remove)
        return

    deficit = max(0, config.max_idle - counters.idle_count)
    to_create = min(deficit, int(config.warmup_concurrency or 1))
    if to_create == 0 or reconcile_state.is_backoff_active(now):
        state_store.renew_primary_lock(pool_name, owner_id, ttl)
        return

    if not state_store.renew_primary_lock(pool_name, owner_id, ttl):
        return

    futures = [warmup_executor.submit(create_one) for _ in range(to_create)]
    wait(futures)
    created_ids: list[str] = []
    failure_count = 0
    last_error: str | None = None
    for future in futures:
        try:
            sandbox_id = future.result()
            if sandbox_id is not None:
                created_ids.append(sandbox_id)
            else:
                failure_count += 1
                last_error = None
        except Exception as exc:
            failure_count += 1
            last_error = str(exc)
    reconcile_state.record_failures(failure_count, last_error)

    created = 0
    for index, sandbox_id in enumerate(created_ids):
        if not state_store.renew_primary_lock(pool_name, owner_id, ttl):
            for orphaned_id in created_ids[index:]:
                _discard(on_discard_sandbox, orphaned_id)
            logger.warning(
                "Reconcile lost primary lock before put_idle; dropped %s newly created sandbox(es): pool_name=%s",
                len(created_ids) - index,
                pool_name,
            )
            return
        try:
            state_store.put_idle(pool_name, sandbox_id)
            created += 1
            reconcile_state.record_success()
        except Exception as exc:
            reconcile_state.record_failure(str(exc))
            for orphaned_id in created_ids[index:]:
                try:
                    state_store.remove_idle(pool_name, orphaned_id)
                except Exception:
                    pass
                _discard(on_discard_sandbox, orphaned_id)
            logger.warning(
                "Reconcile put_idle failed; dropped %s newly created sandbox(es): pool_name=%s error=%s",
                len(created_ids) - index,
                pool_name,
                exc,
            )
            return
    if created > 0:
        logger.debug("Reconcile created %s sandboxes: pool_name=%s", created, pool_name)


def _shrink_excess_idle(
    config: PoolConfig,
    state_store: PoolStateStore,
    on_discard_sandbox: Callable[[str], None],
    to_remove: int,
) -> None:
    pool_name = config.pool_name
    owner_id = str(config.owner_id)
    ttl = config.primary_lock_ttl
    removed = 0
    for _ in range(to_remove):
        if not state_store.renew_primary_lock(pool_name, owner_id, ttl):
            logger.warning(
                "Reconcile lost primary lock before shrinking idle: pool_name=%s removed=%s",
                pool_name,
                removed,
            )
            return
        sandbox_id = state_store.try_take_idle(pool_name)
        if sandbox_id is None:
            return
        _discard(on_discard_sandbox, sandbox_id)
        removed += 1

    state_store.renew_primary_lock(pool_name, owner_id, ttl)
    logger.debug("Reconcile shrunk %s idle sandbox(es): pool_name=%s", removed, pool_name)


def _discard(on_discard_sandbox: Callable[[str], None], sandbox_id: str) -> None:
    try:
        on_discard_sandbox(sandbox_id)
    except Exception as exc:
        logger.warning(
            "Reconcile sandbox cleanup failed: sandbox_id=%s error=%s", sandbox_id, exc
        )
