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
"""Sandbox pool reconciliation logic."""

from __future__ import annotations

import logging
from collections.abc import Callable
from dataclasses import dataclass
from datetime import datetime, timezone

from opensandbox.pool_types import (
    PoolConfig,
    PoolState,
    PoolStateStore,
)
from opensandbox.pool_types import (
    reap_expired_idle_with_min_ttl as _reap_expired_idle_with_min_ttl,
)

logger = logging.getLogger(__name__)


@dataclass
class ReconcileState:
    degraded_threshold: int
    failure_count: int = 0
    state: PoolState = PoolState.HEALTHY
    last_error: str | None = None

    def record_success(self) -> None:
        self.failure_count = 0
        if self.state == PoolState.DEGRADED:
            self.state = PoolState.HEALTHY
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

    def is_backoff_active(self, now: datetime | None = None) -> bool:
        """Compatibility field; fixed create admission replaces replenish backoff."""
        return False


def run_reconcile_tick(
    *,
    config: PoolConfig,
    state_store: PoolStateStore,
    on_discard_sandbox: Callable[[str], None],
    warming_count: int,
    submit_warmups: Callable[[int], None],
    on_primary_acquired: Callable[[], None] = lambda: None,
) -> bool:
    pool_name = config.pool_name
    owner_id = str(config.owner_id)
    ttl = config.primary_lock_ttl

    if not state_store.try_acquire_primary_lock(pool_name, owner_id, ttl):
        logger.debug(f"Reconcile skip (not primary): pool_name={pool_name}")
        return False
    on_primary_acquired()
    _run_primary_replenish_once(
        config=config,
        state_store=state_store,
        on_discard_sandbox=on_discard_sandbox,
        warming_count=warming_count,
        submit_warmups=submit_warmups,
    )
    return True


def _run_primary_replenish_once(
    *,
    config: PoolConfig,
    state_store: PoolStateStore,
    on_discard_sandbox: Callable[[str], None],
    warming_count: int,
    submit_warmups: Callable[[int], None],
) -> None:
    pool_name = config.pool_name
    owner_id = str(config.owner_id)
    ttl = config.primary_lock_ttl
    now = datetime.now(timezone.utc)

    discarded_alive = _reap_expired_idle_with_min_ttl(
        state_store, pool_name, now, config.acquire_min_remaining_ttl
    )
    for sandbox_id in discarded_alive:
        # Reaped near-expiry but server-side TTL has not yet elapsed; kill so the live
        # sandbox does not linger past its pool membership and consume quota.
        on_discard_sandbox(sandbox_id)
    counters = state_store.snapshot_counters(pool_name)
    excess = max(0, counters.idle_count - config.max_idle)
    to_remove = min(excess, int(config.warmup_concurrency or 1))
    if to_remove > 0:
        _shrink_excess_idle(config, state_store, on_discard_sandbox, to_remove)
        return

    deficit = max(0, config.max_idle - counters.idle_count - warming_count)
    to_create = min(deficit, config.warmup_create_qps)
    if to_create == 0:
        state_store.renew_primary_lock(pool_name, owner_id, ttl)
        return

    if not state_store.renew_primary_lock(pool_name, owner_id, ttl):
        return

    submit_warmups(to_create)


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
                f"Reconcile lost primary lock before shrinking idle: pool_name={pool_name} removed={removed}"
            )
            return
        sandbox_id = state_store.try_take_idle(pool_name)
        if sandbox_id is None:
            return
        _discard(on_discard_sandbox, sandbox_id)
        removed += 1

    state_store.renew_primary_lock(pool_name, owner_id, ttl)
    logger.debug(f"Reconcile shrunk {removed} idle sandbox(es): pool_name={pool_name}")


def _discard(on_discard_sandbox: Callable[[str], None], sandbox_id: str) -> None:
    try:
        on_discard_sandbox(sandbox_id)
    except Exception as exc:
        logger.warning(
            f"Reconcile sandbox cleanup failed: sandbox_id={sandbox_id} error={exc}"
        )
