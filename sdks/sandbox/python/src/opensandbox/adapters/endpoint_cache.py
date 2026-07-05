#
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
#
"""
LRU + TTL endpoint cache with inflight deduplication.

Async version uses asyncio.Future for inflight dedup.
Sync version uses threading.Lock + threading.Event.
"""

from __future__ import annotations

import asyncio
import threading
import time
from collections import OrderedDict
from collections.abc import Awaitable, Callable

from opensandbox.models.sandboxes import SandboxEndpoint

DEFAULT_ENDPOINT_CACHE_TTL = 600.0
DEFAULT_ENDPOINT_CACHE_SIZE = 1024


class EndpointCache:
    """Thread-safe LRU+TTL endpoint cache for sync usage."""

    def __init__(
        self,
        maxsize: int = DEFAULT_ENDPOINT_CACHE_SIZE,
        ttl: float = DEFAULT_ENDPOINT_CACHE_TTL,
    ) -> None:
        self._maxsize = max(1, maxsize)
        self._ttl = ttl
        self._cache: OrderedDict[tuple, tuple[SandboxEndpoint, float]] = OrderedDict()
        self._lock = threading.Lock()
        self._inflight: dict[tuple, _InflightEntry] = {}
        self._generation = 0

    def get(self, key: tuple) -> SandboxEndpoint | None:
        with self._lock:
            entry = self._cache.get(key)
            if entry is None:
                return None
            endpoint, expires_at = entry
            if time.monotonic() >= expires_at:
                del self._cache[key]
                return None
            self._cache.move_to_end(key)
            return endpoint

    def put(self, key: tuple, endpoint: SandboxEndpoint) -> None:
        with self._lock:
            self._put_locked(key, endpoint)

    def _put_locked(self, key: tuple, endpoint: SandboxEndpoint) -> None:
        if key in self._cache:
            self._cache.move_to_end(key)
            self._cache[key] = (endpoint, time.monotonic() + self._ttl)
            return
        while len(self._cache) >= self._maxsize:
            self._cache.popitem(last=False)
        self._cache[key] = (endpoint, time.monotonic() + self._ttl)

    def invalidate(self, sandbox_id: str) -> None:
        with self._lock:
            self._generation += 1
            keys_to_remove = [k for k in self._cache if k[0] == sandbox_id]
            for k in keys_to_remove:
                del self._cache[k]
            inflight_to_remove = [k for k in self._inflight if k[0] == sandbox_id]
            for k in inflight_to_remove:
                del self._inflight[k]

    def get_or_fetch(
        self,
        key: tuple,
        fetch: Callable[[], SandboxEndpoint],
    ) -> SandboxEndpoint:
        with self._lock:
            entry = self._cache.get(key)
            if entry is not None:
                endpoint, expires_at = entry
                if time.monotonic() < expires_at:
                    self._cache.move_to_end(key)
                    return endpoint
                del self._cache[key]

            gen_before = self._generation
            if key in self._inflight:
                inf = self._inflight[key]
                is_owner = False
            else:
                inf = _InflightEntry()
                self._inflight[key] = inf
                is_owner = True

        if not is_owner:
            inf.event.wait()
            if inf.error is not None:
                raise inf.error
            return inf.result  # type: ignore[return-value]

        try:
            endpoint = fetch()
            with self._lock:
                if self._generation == gen_before:
                    self._put_locked(key, endpoint)
            inf.result = endpoint
            return endpoint
        except Exception as e:
            inf.error = e
            raise
        finally:
            with self._lock:
                self._inflight.pop(key, None)
            inf.event.set()


class _InflightEntry:
    __slots__ = ("event", "result", "error", "owner")

    def __init__(self) -> None:
        self.event = threading.Event()
        self.result: SandboxEndpoint | None = None
        self.error: Exception | None = None
        self.owner: bool = False


class AsyncEndpointCache:
    """Async LRU+TTL endpoint cache with asyncio.Future-based inflight dedup."""

    def __init__(
        self,
        maxsize: int = DEFAULT_ENDPOINT_CACHE_SIZE,
        ttl: float = DEFAULT_ENDPOINT_CACHE_TTL,
    ) -> None:
        self._maxsize = max(1, maxsize)
        self._ttl = ttl
        self._cache: OrderedDict[tuple, tuple[SandboxEndpoint, float]] = OrderedDict()
        self._inflight: dict[tuple, asyncio.Future[SandboxEndpoint]] = {}
        self._generation = 0

    def get(self, key: tuple) -> SandboxEndpoint | None:
        entry = self._cache.get(key)
        if entry is None:
            return None
        endpoint, expires_at = entry
        if time.monotonic() >= expires_at:
            del self._cache[key]
            return None
        self._cache.move_to_end(key)
        return endpoint

    def put(self, key: tuple, endpoint: SandboxEndpoint) -> None:
        if key in self._cache:
            self._cache.move_to_end(key)
            self._cache[key] = (endpoint, time.monotonic() + self._ttl)
            return
        while len(self._cache) >= self._maxsize:
            self._cache.popitem(last=False)
        self._cache[key] = (endpoint, time.monotonic() + self._ttl)

    def invalidate(self, sandbox_id: str) -> None:
        self._generation += 1
        keys_to_remove = [k for k in self._cache if k[0] == sandbox_id]
        for k in keys_to_remove:
            del self._cache[k]
        inflight_to_remove = [k for k in self._inflight if k[0] == sandbox_id]
        for k in inflight_to_remove:
            del self._inflight[k]

    async def get_or_fetch(
        self,
        key: tuple,
        fetch: Callable[[], Awaitable[SandboxEndpoint]],
    ) -> SandboxEndpoint:
        cached = self.get(key)
        if cached is not None:
            return cached

        if key in self._inflight:
            return await asyncio.shield(self._inflight[key])

        gen_before = self._generation
        loop = asyncio.get_running_loop()
        future: asyncio.Future[SandboxEndpoint] = loop.create_future()
        self._inflight[key] = future

        try:
            endpoint = await fetch()
            if self._generation == gen_before:
                self.put(key, endpoint)
            future.set_result(endpoint)
            return endpoint
        except BaseException as e:
            if not future.done():
                future.set_exception(e)
            raise
        finally:
            self._inflight.pop(key, None)
