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

"""HTTP-based TenantProvider with per-key in-memory TTL cache.

Endpoint contract:
    GET {endpoint}
    Header: OPEN-SANDBOX-API-KEY: <api_key>

    200 OK:
        {
            "namespace": "ns-a",
            "ttl": 60
        }
        - namespace: target K8s namespace for this key
        - ttl: suggested cache duration in seconds

    401 Unauthorized:
        {
            "code": "UNAUTHORIZED",
            "message": "..."
        }

Cache strategy:
    - Per-key cache entry with server-suggested TTL
    - lookup hit + within TTL → return cached
    - lookup hit + TTL expired → sync GET → refresh or serve stale within max_stale
    - lookup miss → sync GET → 200: cache + return; 401: return None
    - Network failure + beyond max_stale → raise TenantProviderUnavailable
"""

from __future__ import annotations

import logging
import threading
import time
from dataclasses import dataclass
from typing import Callable, Dict, List, Optional

import httpx

from opensandbox_server.tenants.models import TenantEntry
from opensandbox_server.tenants.provider import TenantProviderUnavailable

logger = logging.getLogger(__name__)


@dataclass
class HTTPTenantProviderConfig:
    endpoint: str
    max_stale_seconds: float = 300.0
    timeout_seconds: float = 5.0
    auth_header: Optional[str] = None
    auth_token: Optional[str] = None


@dataclass
class _CacheEntry:
    tenant: TenantEntry
    fetched_at: float
    ttl: float


class _FlightEvent(threading.Event):
    """Event with attached result/error for singleflight propagation."""

    result: Optional[TenantEntry] = None
    error: Optional[Exception] = None


class HTTPTenantProvider:
    """TenantProvider backed by a remote HTTP endpoint with per-key TTL cache.

    Each lookup that misses or expires in cache triggers a sync GET to the
    remote endpoint. The server response includes a suggested TTL for caching.
    Uses per-key locks to prevent thundering herd on TTL expiry.
    """

    def __init__(self, config: HTTPTenantProviderConfig) -> None:
        self._config = config
        self._lock = threading.Lock()
        self._cache: Dict[str, _CacheEntry] = {}
        self._inflight: Dict[str, _FlightEvent] = {}
        self._ready = False
        self._callbacks: List[Callable[[List[TenantEntry]], None]] = []
        self._client: Optional[httpx.Client] = None

    def lookup(self, api_key: str) -> Optional[TenantEntry]:
        now = time.monotonic()

        with self._lock:
            cached = self._cache.get(api_key)

        if cached is not None:
            age = now - cached.fetched_at
            if age <= cached.ttl:
                return cached.tenant

            # TTL expired — sync refresh
            try:
                return self._fetch_and_cache(api_key, now)
            except _Unauthorized:
                with self._lock:
                    self._cache.pop(api_key, None)
                return None
            except Exception:
                if age > cached.ttl + self._config.max_stale_seconds:
                    raise TenantProviderUnavailable(
                        f"HTTP tenant endpoint unreachable and cache stale "
                        f"beyond {self._config.max_stale_seconds}s"
                    )
                logger.warning("HTTP tenant fetch failed, serving stale entry (age=%.1fs)", age)
                return cached.tenant

        # Cache miss — sync fetch
        try:
            return self._fetch_and_cache(api_key, now)
        except _Unauthorized:
            return None
        except Exception as e:
            raise TenantProviderUnavailable(f"HTTP tenant endpoint unreachable: {e}") from e

    def list_tenants(self) -> List[TenantEntry]:
        with self._lock:
            seen = {}
            for entry in self._cache.values():
                seen[entry.tenant.name] = entry.tenant
            return list(seen.values())

    def ready(self) -> bool:
        return self._ready

    def start(self) -> None:
        if self._config.endpoint and not self._config.endpoint.startswith("https://"):
            logger.warning(
                "HTTP tenant endpoint is not HTTPS (%s). "
                "API keys will be transmitted in cleartext.",
                self._config.endpoint,
            )
        self._client = httpx.Client(timeout=self._config.timeout_seconds)
        self._ready = True
        logger.info("HTTP tenant provider started, endpoint=%s", self._config.endpoint)

    def close(self) -> None:
        with self._lock:
            self._cache.clear()
            self._ready = False
        if self._client:
            self._client.close()
            self._client = None

    def on_reload(self, callback: Callable[[List[TenantEntry]], None]) -> None:
        self._callbacks.append(callback)

    def _fetch_and_cache(self, api_key: str, now: float) -> Optional[TenantEntry]:
        """Singleflight GET: only one fetch per key at a time, others wait.

        Propagates leader result (entry or exception) to all waiters so
        provider outages / 5xx errors don't masquerade as invalid credentials.
        """
        with self._lock:
            flight = self._inflight.get(api_key)
            if flight is not None:
                is_leader = False
            else:
                flight = _FlightEvent()
                self._inflight[api_key] = flight
                is_leader = True

        if not is_leader:
            flight.wait(timeout=self._config.timeout_seconds)
            if flight.error is not None:
                raise flight.error
            with self._lock:
                cached = self._cache.get(api_key)
            if cached:
                return cached.tenant
            raise TenantProviderUnavailable("Timed out waiting for in-flight tenant lookup")

        try:
            return self._do_fetch(api_key, now)
        except Exception as e:
            flight.error = e
            raise
        finally:
            with self._lock:
                self._inflight.pop(api_key, None)
            flight.set()

    def _do_fetch(self, api_key: str, now: float) -> Optional[TenantEntry]:
        """GET the endpoint for a single api_key. Returns TenantEntry or raises."""
        assert self._client is not None

        headers: Dict[str, str] = {"OPEN-SANDBOX-API-KEY": api_key}
        if self._config.auth_header and self._config.auth_token:
            headers[self._config.auth_header] = self._config.auth_token

        resp = self._client.get(self._config.endpoint, headers=headers)

        if resp.status_code == 401:
            raise _Unauthorized()

        resp.raise_for_status()

        data = resp.json()
        namespace = (data.get("namespace") or "").strip()
        if not namespace:
            raise ValueError("HTTP tenant endpoint returned empty namespace for key")
        ttl = float(data.get("ttl", 30))

        entry = TenantEntry(
            name=namespace,
            namespace=namespace,
            api_keys=(api_key,),
        )

        with self._lock:
            self._cache[api_key] = _CacheEntry(tenant=entry, fetched_at=now, ttl=ttl)

        return entry


class _Unauthorized(Exception):
    pass
