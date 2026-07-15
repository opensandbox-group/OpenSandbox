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

"""Single renew-intent pipeline: Redis BRPOP feeders + proxy submits → one asyncio queue → processors."""

from __future__ import annotations

import asyncio
import logging
import time
from collections import OrderedDict
from dataclasses import dataclass
from datetime import datetime, timezone
from functools import partial
from typing import TYPE_CHECKING, Optional

from redis.exceptions import RedisError

from opensandbox_server.config import AppConfig
from opensandbox_server.integrations.renew_intent.constants import (
    BRPOP_TIMEOUT_SECONDS,
    INTENT_MAX_AGE_SECONDS,
    PROXY_RENEW_MAX_TRACKED_SANDBOXES,
)
from opensandbox_server.integrations.renew_intent.controller import AccessRenewController
from opensandbox_server.integrations.renew_intent.intent import parse_renew_intent_json
from opensandbox_server.integrations.renew_intent.logutil import (
    RENEW_EVENT_WORKERS_NOT_STARTED,
    RENEW_EVENT_WORKERS_STARTED,
    RENEW_SOURCE_REDIS_QUEUE,
    RENEW_SOURCE_SERVER_PROXY,
    renew_bundle,
)
from opensandbox_server.integrations.renew_intent.redis_client import connect_renew_intent_redis_from_config
from opensandbox_server.services.extension_service import ExtensionService, require_extension_service
from opensandbox_server.services.factory import create_sandbox_service
from opensandbox_server.services.sandbox_service import SandboxService

if TYPE_CHECKING:
    from redis.asyncio import Redis

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class RenewWorkItem:
    """One unit of work for the shared renew pipeline."""

    source: str
    sandbox_id: str
    observed_at: datetime


@dataclass
class _MemSandboxState:
    lock: asyncio.Lock
    last_success_monotonic: float | None = None


class RenewIntentConsumer:
    """
    Feeds renew work from Redis BRPOP (optional) and server-proxy ``schedule`` into one queue.
    Per-sandbox ``asyncio.Lock`` serializes work; without Redis, ``min_interval`` throttles proxy
    renews (ingress throttling is producer-side).
    """

    def __init__(
        self,
        app_config: AppConfig,
        sandbox_service: SandboxService,
        extension_service: ExtensionService,
        redis_client: Optional["Redis"],
    ) -> None:
        self._app_config = app_config
        self._redis = redis_client
        ri = app_config.renew_intent
        self._queue_key = ri.redis.queue_key
        self._feeder_count = ri.redis.consumer_concurrency if redis_client else 0
        self._processor_count = max(1, ri.redis.consumer_concurrency)
        self._min_interval = float(ri.min_interval_seconds)
        self._controller = AccessRenewController(sandbox_service, extension_service)
        self._work_queue: asyncio.Queue[RenewWorkItem] = asyncio.Queue()
        self._stop = asyncio.Event()
        self._tasks: list[asyncio.Task[None]] = []
        self._mem_states: OrderedDict[str, _MemSandboxState] = OrderedDict()
        self._max_tracked = PROXY_RENEW_MAX_TRACKED_SANDBOXES

    @classmethod
    async def start(
        cls,
        app_config: AppConfig,
        sandbox_service: SandboxService,
        extension_service: ExtensionService,
    ) -> Optional["RenewIntentConsumer"]:
        if not app_config.renew_intent.enabled:
            return None

        redis_client: Optional["Redis"] = None
        if app_config.renew_intent.redis.enabled:
            try:
                redis_client = await connect_renew_intent_redis_from_config(app_config)
            except (RedisError, OSError, TimeoutError) as exc:
                line, ex = renew_bundle(
                    event=RENEW_EVENT_WORKERS_NOT_STARTED,
                    source=RENEW_SOURCE_REDIS_QUEUE,
                    skip_reason="redis_connect_failed",
                    error_type=type(exc).__name__,
                )
                logger.error(f"renew_intent {line} error={exc!s}", extra=ex)
                redis_client = None
            if redis_client is None and app_config.renew_intent.redis.enabled:
                line, ex = renew_bundle(
                    event=RENEW_EVENT_WORKERS_NOT_STARTED,
                    source=RENEW_SOURCE_REDIS_QUEUE,
                    skip_reason="redis_client_none",
                )
                logger.warning(
                    f"renew_intent {line}; continuing with proxy-only renew pipeline",
                    extra=ex,
                )

        consumer = cls(app_config, sandbox_service, extension_service, redis_client)
        consumer._spawn_tasks()
        if redis_client is not None:
            line, ex = renew_bundle(
                event=RENEW_EVENT_WORKERS_STARTED,
                source=RENEW_SOURCE_REDIS_QUEUE,
                worker_count=consumer._feeder_count + consumer._processor_count,
                queue_key=consumer._queue_key,
            )
            logger.info(
                f"🧪 [EXPERIMENTAL] renew_intent is enabled: Redis BRPOP feeders + "
                f"unified processors started ({line})",
                extra=ex,
            )
        else:
            logger.info(
                "🧪 [EXPERIMENTAL] renew_intent is enabled: unified in-process renew pipeline "
                "(proxy path only; no Redis BRPOP)"
            )
        return consumer

    def submit_from_proxy(self, sandbox_id: str) -> None:
        """Enqueue renew work from ``/sandboxes/.../proxy/...`` (non-blocking)."""
        if not self._app_config.renew_intent.enabled:
            return
        asyncio.create_task(
            self._enqueue_proxy(sandbox_id),
            name=f"renew_intent_proxy_enqueue_{sandbox_id}",
        )

    async def _enqueue_proxy(self, sandbox_id: str) -> None:
        await self._work_queue.put(
            RenewWorkItem(
                source=RENEW_SOURCE_SERVER_PROXY,
                sandbox_id=sandbox_id,
                observed_at=datetime.now(timezone.utc),
            )
        )

    def _spawn_tasks(self) -> None:
        for i in range(self._processor_count):
            self._tasks.append(
                asyncio.create_task(
                    self._processor_loop(i),
                    name=f"renew_intent_processor_{i}",
                )
            )
        for i in range(self._feeder_count):
            self._tasks.append(
                asyncio.create_task(
                    self._brpop_feeder_loop(i),
                    name=f"renew_intent_brpop_{i}",
                )
            )

    @staticmethod
    def _is_stale(observed_at: datetime) -> bool:
        now = datetime.now(timezone.utc)
        age = (now - observed_at).total_seconds()
        return age > INTENT_MAX_AGE_SECONDS

    def _ensure_mru_mem(self, sandbox_id: str) -> _MemSandboxState:
        if sandbox_id in self._mem_states:
            st = self._mem_states[sandbox_id]
            self._mem_states.move_to_end(sandbox_id)
        else:
            st = _MemSandboxState(lock=asyncio.Lock())
            self._mem_states[sandbox_id] = st
            self._mem_states.move_to_end(sandbox_id)
        self._evict_mem_lru_unlocked()
        return st

    def _evict_mem_lru_unlocked(self) -> None:
        rotations = 0
        max_rotations = max(len(self._mem_states), 1)
        while len(self._mem_states) > self._max_tracked and rotations < max_rotations:
            k, st = self._mem_states.popitem(last=False)
            if st.lock.locked():
                self._mem_states[k] = st
                self._mem_states.move_to_end(k)
                rotations += 1
            else:
                rotations = 0

    async def _brpop_feeder_loop(self, worker_id: int) -> None:
        assert self._redis is not None
        while not self._stop.is_set():
            try:
                result = await self._redis.brpop(
                    self._queue_key,
                    BRPOP_TIMEOUT_SECONDS,
                )
            except asyncio.CancelledError:
                raise
            except (RedisError, OSError) as exc:
                line, ex = renew_bundle(
                    event="worker_redis_error",
                    source=RENEW_SOURCE_REDIS_QUEUE,
                    worker_id=worker_id,
                    error_type=type(exc).__name__,
                )
                logger.warning(f"renew_intent {line} error={exc!s}", extra=ex)
                await asyncio.sleep(1.0)
                continue

            if result is None:
                continue
            _, payload = result
            if not isinstance(payload, str):
                continue
            try:
                intent = parse_renew_intent_json(payload)
                if intent is None:
                    continue
                if self._is_stale(intent.observed_at):
                    continue
                await self._work_queue.put(
                    RenewWorkItem(
                        source=RENEW_SOURCE_REDIS_QUEUE,
                        sandbox_id=intent.sandbox_id,
                        observed_at=intent.observed_at,
                    )
                )
            except Exception as exc:
                line, ex = renew_bundle(
                    event="worker_handle_error",
                    source=RENEW_SOURCE_REDIS_QUEUE,
                    worker_id=worker_id,
                    error_type=type(exc).__name__,
                )
                logger.exception(f"renew_intent {line} error={exc!s}", extra=ex)

    async def _processor_loop(self, worker_id: int) -> None:
        while not self._stop.is_set():
            try:
                work = await asyncio.wait_for(self._work_queue.get(), timeout=1.0)
            except asyncio.TimeoutError:
                continue
            except asyncio.CancelledError:
                raise
            try:
                await self._process_work(work)
            except Exception as exc:
                line, ex = renew_bundle(
                    event="processor_error",
                    source=work.source,
                    sandbox_id=work.sandbox_id,
                    worker_id=worker_id,
                    error_type=type(exc).__name__,
                )
                logger.exception(f"renew_intent {line} error={exc!s}", extra=ex)
            finally:
                self._work_queue.task_done()

    async def _process_work(self, work: RenewWorkItem) -> None:
        if self._redis is None and work.source != RENEW_SOURCE_SERVER_PROXY:
            return

        st = self._ensure_mru_mem(work.sandbox_id)
        async with st.lock:
            if self._redis is not None:
                await self._controller.renew_after_gates(work.sandbox_id, source=work.source)
                return

            now = time.monotonic()
            if (
                st.last_success_monotonic is not None
                and (now - st.last_success_monotonic) < self._min_interval
            ):
                return
            ok = await asyncio.to_thread(
                partial(
                    self._controller.attempt_renew_sync,
                    work.sandbox_id,
                    source=work.source,
                )
            )
            if ok:
                st.last_success_monotonic = time.monotonic()

    async def stop(self) -> None:
        self._stop.set()
        for t in self._tasks:
            t.cancel()
        await asyncio.gather(*self._tasks, return_exceptions=True)
        self._tasks.clear()
        if self._redis is not None:
            try:
                await self._redis.aclose()
            except Exception as exc:
                logger.debug(f"renew_intent redis_close error={exc!s}")


async def start_renew_intent_consumer(
    app_config: AppConfig,
    sandbox_service: SandboxService | None = None,
    extension_service: ExtensionService | None = None,
) -> Optional[RenewIntentConsumer]:
    """Start consumer or ``None`` when ``renew_intent.enabled`` is false."""
    if sandbox_service is None:
        sandbox_service = create_sandbox_service(config=app_config)
    if extension_service is None:
        extension_service = require_extension_service(sandbox_service)
    return await RenewIntentConsumer.start(app_config, sandbox_service, extension_service)
