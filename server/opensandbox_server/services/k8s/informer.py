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

"""Lightweight informer-style cache for namespaced custom resources."""

import logging
import math
import threading
import time
from typing import Any, Callable, Dict, List, Optional

from kubernetes import watch
from kubernetes.client import ApiException

logger = logging.getLogger(__name__)

# An idle watch sends nothing until the server closes the stream at
# ``timeout_seconds``, so the client read timeout must sit above it.
_WATCH_READ_TIMEOUT_BUFFER_SECONDS = 10
_WATCH_CONNECT_TIMEOUT_SECONDS = 10
_MAX_BACKOFF_SECONDS = 30.0


class WorkloadInformer:
    """Maintain a LIST/WATCH-owned cache of a namespaced custom resource."""

    def __init__(
        self,
        list_fn: Callable[..., Dict[str, Any]],
        resync_period_seconds: int = 300,
        watch_timeout_seconds: int = 60,
        enable_watch: bool = True,
        thread_name: str = "workload-informer",
    ):
        """
        Args:
            list_fn: Callable that lists the custom resource, with signature
                     ``list_fn(**kwargs) -> dict``.  Typically a bound method
                     like ``custom_api.list_namespaced_custom_object``.
            resync_period_seconds: Full-resync interval for the cache.
            watch_timeout_seconds: Per-stream watch timeout before restart.
            enable_watch: When False only the initial list is performed.
            thread_name: Name for the background thread, used in stack traces
                         and debuggers.  Should be unique per informer instance.
        """
        self.list_fn = list_fn
        self.resync_period_seconds = resync_period_seconds
        self.watch_timeout_seconds = watch_timeout_seconds
        self.enable_watch = enable_watch
        self._thread_name = thread_name

        self._cache: Dict[str, Dict[str, Any]] = {}
        self._lock = threading.RLock()
        self._resource_version: Optional[str] = None
        self._has_synced = False
        self._last_contact_at: Optional[float] = None
        self._invalidation_generation = 0
        self._stop_event = threading.Event()
        self._thread: Optional[threading.Thread] = None

    @property
    def _staleness_limit_seconds(self) -> float:
        """One resync period, by which the cache should have been rebuilt, plus a watch cycle."""
        return self.resync_period_seconds + self.watch_timeout_seconds

    def start(self) -> None:
        """Start the background watch thread if not already running."""
        if self._stop_event.is_set():
            return
        if self._thread and self._thread.is_alive():
            return

        self._thread = threading.Thread(
            target=self._run,
            name=self._thread_name,
            daemon=True,
        )
        self._thread.start()

    def stop(self) -> None:
        """Stop the background watch thread."""
        self._stop_event.set()

    def get_if_synced(self, name: str) -> Optional[Dict[str, Any]]:
        """Return a cached object only while the cache is safe to publish."""
        with self._lock:
            if not self._cache_is_available():
                return None
            return self._cache.get(name)

    def list_if_synced(self) -> Optional[List[Dict[str, Any]]]:
        """Return a cache snapshot, or None when callers must use the live API."""
        with self._lock:
            if not self._cache_is_available():
                return None
            return list(self._cache.values())

    def _cache_is_available(self) -> bool:
        """Return whether cache reads are publishable. Caller must hold ``_lock``."""
        if self._stop_event.is_set() or not self._has_synced:
            return False
        if self._last_contact_at is None:
            return False
        return time.monotonic() - self._last_contact_at <= self._staleness_limit_seconds

    def invalidate(self) -> None:
        """Make the published cache unavailable after a successful direct mutation.

        The cached objects and watch cursor remain owned exclusively by LIST/WATCH.
        A generation change also prevents an in-flight LIST from publishing a
        snapshot that may not include the completed mutation.
        """
        with self._lock:
            self._invalidation_generation += 1
            self._has_synced = False

    def _run(self) -> None:
        backoff = 1.0
        last_full_resync_at: Optional[float] = None
        while not self._stop_event.is_set():
            try:
                if not self._has_synced:
                    published = self._full_resync()
                    if not published:
                        logger.debug("Informer full resync invalidated before publication")
                        backoff = self._wait_before_retry(backoff)
                        continue
                    last_full_resync_at = time.monotonic()
                    backoff = 1.0

                if not self.enable_watch:
                    self._stop_event.wait(self.resync_period_seconds)
                    self._has_synced = False  # trigger a fresh list on next loop
                    continue

                if last_full_resync_at is None:
                    last_full_resync_at = time.monotonic()
                remaining_resync_seconds = self.resync_period_seconds - (
                    time.monotonic() - last_full_resync_at
                )
                if remaining_resync_seconds <= 0:
                    self._has_synced = False
                    continue

                watch_timeout_seconds = min(
                    self.watch_timeout_seconds,
                    max(1, math.ceil(remaining_resync_seconds)),
                )
                self._run_watch_loop(watch_timeout_seconds)
                if time.monotonic() - last_full_resync_at >= self.resync_period_seconds:
                    self._has_synced = False
                backoff = 1.0
            except ApiException as exc:
                if exc.status == 410:
                    # Resource version too old; force a fresh list on next loop.
                    self._resource_version = None
                    self._has_synced = False
                else:
                    logger.warning(f"Informer watch error: {exc}", exc_info=True)
                    self._has_synced = False
                    backoff = self._wait_before_retry(backoff)
            except Exception as exc:  # pragma: no cover - defensive
                logger.warning(f"Unexpected informer error: {exc}", exc_info=True)
                self._has_synced = False
                backoff = self._wait_before_retry(backoff)

    def _wait_before_retry(self, backoff: float) -> float:
        """Wait interruptibly and return the next bounded retry delay."""
        self._stop_event.wait(backoff)
        return min(backoff * 2, _MAX_BACKOFF_SECONDS)

    def _full_resync(self) -> bool:
        """Atomically publish a full LIST snapshot unless a mutation invalidated it."""
        with self._lock:
            start_generation = self._invalidation_generation

        resp = self.list_fn()

        # list response is a dict for CustomObjectsApi
        items = resp.get("items", [])
        metadata = resp.get("metadata", {})
        resource_version = metadata.get("resourceVersion")

        # Build new cache outside the lock to avoid blocking readers
        new_cache: Dict[str, Dict[str, Any]] = {}
        for item in items:
            name = item.get("metadata", {}).get("name")
            if name:
                new_cache[name] = item

        with self._lock:
            if self._invalidation_generation != start_generation:
                return False

            self._cache = new_cache
            self._resource_version = resource_version
            self._has_synced = True
            self._last_contact_at = time.monotonic()
            return True

    def _run_watch_loop(self, timeout_seconds: int) -> None:
        """Stream watch events to keep the cache fresh."""
        w = watch.Watch()
        try:
            for event in w.stream(
                self.list_fn,
                resource_version=self._resource_version,
                timeout_seconds=timeout_seconds,
                # Without this a half-open connection parks the thread forever.
                _request_timeout=(
                    _WATCH_CONNECT_TIMEOUT_SECONDS,
                    timeout_seconds + _WATCH_READ_TIMEOUT_BUFFER_SECONDS,
                ),
            ):
                if self._stop_event.is_set():
                    break
                if not isinstance(event, dict):
                    raise TypeError("Informer watch returned a non-dict event")
                self._handle_event(event)
        finally:
            w.stop()

        # The stream ran to completion, so the API server is still reachable.
        with self._lock:
            self._last_contact_at = time.monotonic()

    def _handle_event(self, event: Dict[str, Any]) -> None:
        obj = event.get("object")
        if obj is None:
            return

        if not isinstance(obj, dict):
            try:
                obj = obj.to_dict()
            except Exception:
                return

        metadata = obj.get("metadata", {})
        name = metadata.get("name")
        if not name:
            return

        event_type = event.get("type")
        with self._lock:
            if event_type == "DELETED":
                self._cache.pop(name, None)
            else:
                self._cache[name] = obj
            resource_version = metadata.get("resourceVersion")
            if resource_version:
                # resourceVersion is opaque. Watch stream order, not numeric
                # comparison, determines the last consumed cursor.
                self._resource_version = resource_version
            else:
                # The cache update is retained internally, but cannot be
                # published until a LIST supplies a trustworthy cursor again.
                self._has_synced = False
