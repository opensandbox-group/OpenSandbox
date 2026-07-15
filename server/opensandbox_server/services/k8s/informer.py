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
import threading
from typing import Any, Callable, Dict, List, Optional

from kubernetes import watch
from kubernetes.client import ApiException

logger = logging.getLogger(__name__)


class WorkloadInformer:
    """Maintain an in-memory cache of a namespaced custom resource via watch."""

    def __init__(
        self,
        list_fn: Callable[..., Any],
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
            resync_period_seconds: Full-resync interval when watch is disabled.
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
        self._stop_event = threading.Event()
        self._thread: Optional[threading.Thread] = None

    @property
    def has_synced(self) -> bool:
        """Return True once an initial list has completed."""
        return self._has_synced

    def start(self) -> None:
        """Start the background watch thread if not already running."""
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

    def get(self, name: str) -> Optional[Dict[str, Any]]:
        """Return cached object by name, if present."""
        with self._lock:
            return self._cache.get(name)

    def list(self) -> List[Dict[str, Any]]:
        """Return a snapshot of every cached object."""
        with self._lock:
            return list(self._cache.values())

    def update_cache(self, obj: Dict[str, Any]) -> None:
        """Upsert a single object into the cache.

        Only advances ``_resource_version`` if the incoming version is strictly
        newer, preventing a stale API response from rolling back the watch cursor.
        """
        metadata = obj.get("metadata", {})
        name = metadata.get("name")
        if not name:
            return

        with self._lock:
            self._cache[name] = obj
            self._advance_resource_version(metadata.get("resourceVersion"))

    def delete_from_cache(self, name: str) -> None:
        """Evict a single object from the cache by name."""
        with self._lock:
            self._cache.pop(name, None)

    def _advance_resource_version(self, rv: Optional[str]) -> None:
        """Advance ``_resource_version`` only when *rv* is strictly newer.

        K8s resourceVersions are opaque strings but etcd encodes them as
        monotonically increasing integers.  If the conversion fails we skip the
        update (conservative: keep the current, newer cursor).

        Must be called with ``self._lock`` already held.
        """
        if not rv:
            return
        if self._resource_version is None:
            self._resource_version = rv
            return
        try:
            if int(rv) > int(self._resource_version):
                self._resource_version = rv
        except ValueError:
            # Non-integer resourceVersion — skip to avoid downgrade.
            pass

    def _run(self) -> None:
        backoff = 1.0
        while not self._stop_event.is_set():
            try:
                if not self._has_synced:
                    self._full_resync()
                    backoff = 1.0

                if not self.enable_watch:
                    self._stop_event.wait(self.resync_period_seconds)
                    self._has_synced = False  # trigger a fresh list on next loop
                    continue

                self._run_watch_loop()
                backoff = 1.0
            except ApiException as exc:
                if exc.status == 410:
                    # Resource version too old; force a fresh list on next loop.
                    self._resource_version = None
                    self._has_synced = False
                else:
                    logger.warning(f"Informer watch error: {exc}", exc_info=True)
                    self._has_synced = False
                    self._stop_event.wait(min(backoff, 30.0))
                    backoff = min(backoff * 2, 30.0)
            except Exception as exc:  # pragma: no cover - defensive
                logger.warning(f"Unexpected informer error: {exc}", exc_info=True)
                self._has_synced = False
                self._stop_event.wait(min(backoff, 30.0))
                backoff = min(backoff * 2, 30.0)

    def _full_resync(self) -> None:
        """Perform a full list to refresh the cache."""
        resp = self.list_fn()

        # list response is a dict for CustomObjectsApi
        items = resp.get("items", []) if isinstance(resp, dict) else []
        metadata = resp.get("metadata", {}) if isinstance(resp, dict) else {}
        resource_version = metadata.get("resourceVersion")

        # Build new cache outside the lock to avoid blocking readers
        new_cache: Dict[str, Dict[str, Any]] = {}
        for item in items:
            name = item.get("metadata", {}).get("name")
            if name:
                new_cache[name] = item

        with self._lock:
            self._cache = new_cache
            self._advance_resource_version(resource_version)
            self._has_synced = True

    def _run_watch_loop(self) -> None:
        """Stream watch events to keep the cache fresh."""
        w = watch.Watch()
        try:
            for event in w.stream(
                self.list_fn,
                resource_version=self._resource_version,
                timeout_seconds=self.watch_timeout_seconds,
            ):
                if self._stop_event.is_set():
                    break
                self._handle_event(event)
        finally:
            w.stop()

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
            self._advance_resource_version(metadata.get("resourceVersion"))
