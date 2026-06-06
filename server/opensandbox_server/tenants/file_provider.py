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

from __future__ import annotations

import logging
import os
import threading
from pathlib import Path
from typing import Callable, Dict, List, Optional

from opensandbox_server.tenants.models import TenantEntry

try:
    import tomllib
except ModuleNotFoundError:
    import tomli as tomllib  # type: ignore[import]

logger = logging.getLogger(__name__)

TENANTS_CONFIG_ENV_VAR = "SANDBOX_TENANTS_CONFIG_PATH"
DEFAULT_TENANTS_CONFIG_PATH = Path.home() / ".opensandbox" / "tenants.toml"


def resolve_tenants_path(path: Optional[str | Path] = None) -> Path:
    if path:
        return Path(path)
    env = os.environ.get(TENANTS_CONFIG_ENV_VAR)
    if env:
        return Path(env)
    return DEFAULT_TENANTS_CONFIG_PATH


def _parse_tenants_file(path: Path) -> List[TenantEntry]:
    with open(path, "rb") as f:
        data = tomllib.load(f)

    entries: List[TenantEntry] = []
    seen_keys: Dict[str, str] = {}

    for raw in data.get("tenants", []):
        name = raw["name"]
        namespace = raw["namespace"]
        api_keys = raw["api_keys"]

        if not api_keys:
            raise ValueError(f"Tenant '{name}' has no api_keys configured.")

        for key in api_keys:
            if key in seen_keys:
                raise ValueError(
                    f"Duplicate api_key across tenants: '{name}' and '{seen_keys[key]}'."
                )
            seen_keys[key] = name

        entries.append(TenantEntry(name=name, namespace=namespace, api_keys=tuple(api_keys)))

    return entries


def _build_lookup_dict(entries: List[TenantEntry]) -> Dict[str, TenantEntry]:
    result: Dict[str, TenantEntry] = {}
    for entry in entries:
        for key in entry.api_keys:
            result[key] = entry
    return result


class FileTenantProvider:
    """TenantProvider backed by a local tenants.toml file with hot-reload via filesystem polling."""

    def __init__(self, path: Optional[str | Path] = None) -> None:
        self._path = resolve_tenants_path(path)
        self._lock = threading.Lock()
        self._lookup: Dict[str, TenantEntry] = {}
        self._entries: List[TenantEntry] = []
        self._ready = False
        self._callbacks: List[Callable[[List[TenantEntry]], None]] = []
        self._watcher_stop = threading.Event()
        self._watcher_thread: Optional[threading.Thread] = None

    @property
    def path(self) -> Path:
        return self._path

    def lookup(self, api_key: str) -> Optional[TenantEntry]:
        with self._lock:
            return self._lookup.get(api_key)

    def list_tenants(self) -> List[TenantEntry]:
        with self._lock:
            return list(self._entries)

    def ready(self) -> bool:
        return self._ready

    def start(self) -> None:
        self._load()
        self._watcher_thread = threading.Thread(
            target=self._watch_loop, daemon=True, name="tenant-file-watcher"
        )
        self._watcher_thread.start()

    def close(self) -> None:
        self._watcher_stop.set()
        if self._watcher_thread and self._watcher_thread.is_alive():
            self._watcher_thread.join(timeout=5)

    def on_reload(self, callback: Callable[[List[TenantEntry]], None]) -> None:
        self._callbacks.append(callback)

    def _load(self) -> None:
        if not self._path.exists():
            raise FileNotFoundError(f"Tenants config not found: {self._path}")

        entries = _parse_tenants_file(self._path)
        lookup = _build_lookup_dict(entries)

        with self._lock:
            self._entries = entries
            self._lookup = lookup
            self._ready = True

        logger.info("Loaded %d tenant(s) from %s", len(entries), self._path)

    def _reload(self) -> None:
        try:
            entries = _parse_tenants_file(self._path)
            lookup = _build_lookup_dict(entries)

            with self._lock:
                self._entries = entries
                self._lookup = lookup

            logger.info("Reloaded %d tenant(s) from %s", len(entries), self._path)
            for cb in self._callbacks:
                try:
                    cb(entries)
                except Exception:
                    logger.exception("Tenant reload callback failed")

        except FileNotFoundError:
            with self._lock:
                self._entries = []
                self._lookup = {}
            logger.warning("Tenants config deleted: %s — all tenant keys invalidated", self._path)

        except Exception:
            logger.exception("Failed to reload tenants config — keeping previous state")

    def _watch_loop(self) -> None:
        last_mtime: Optional[float] = None
        try:
            last_mtime = self._path.stat().st_mtime
        except OSError:
            pass

        while not self._watcher_stop.wait(timeout=2.0):
            try:
                current_mtime = self._path.stat().st_mtime
            except OSError:
                if last_mtime is not None:
                    self._reload()
                    last_mtime = None
                continue

            if last_mtime is None or current_mtime != last_mtime:
                last_mtime = current_mtime
                self._reload()
