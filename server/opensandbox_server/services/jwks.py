# Copyright 2026 The OpenSandbox Authors
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

"""JWKS publication for egress caller-authentication keys.

Serves one public Ed25519 key per kid at ``GET /credential-vault/jwks/{kid}``
— public keys only, so the endpoint needs no authentication (the OIDC
jwks.json posture); abuse is bounded by kid-grammar rejection and rate
limiting. Fast-sandbox pool keys come from the configured signing Secrets;
sidecar keys come from the per-sandbox workload records.
"""

from __future__ import annotations

import base64
import logging
import re
import threading
import time
from typing import Callable, Mapping, Optional, Protocol

logger = logging.getLogger(__name__)

#: Platform identifier grammar — identical to the egress mint side and the
#: resolver SDK (frozen in the shared vectors' contract block). Off-grammar
#: kids are rejected locally: no Secret read, no rate-budget consumption.
KID_GRAMMAR = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]{0,62}(:v[0-9]{1,8})?$")

#: Signing Secret entries.
SIGNING_PUBLIC_KEY_ENTRY = "signing.pub"
KID_ENTRY = "kid"

DEFAULT_CACHE_TTL_SECONDS = 45.0
DEFAULT_MAX_LOOKUPS = 120
DEFAULT_WINDOW_SECONDS = 60.0

_ED25519_PUBLIC_KEY_BYTES = 32


class RateLimited(Exception):
    """The lookup budget for the current window is exhausted (HTTP 429)."""


class SecretReader(Protocol):
    """Returns one Secret's data mapping (bytes values), or None if absent."""

    def __call__(self, name: str) -> Optional[Mapping[str, bytes]]: ...


class SidecarKeyReader(Protocol):
    """Returns the per-sandbox JWK recorded on a workload, or None.

    None means authoritative absence (no such sandbox / no recorded JWK).
    Transient backend failures should raise; the service surfaces them as
    retryable errors, never as absence.
    """

    def __call__(self, sandbox_id: str) -> Optional[dict]: ...


class JWKSBackendError(Exception):
    """A transient backend failure (Secret read, workload lookup).

    Surfaced by the route as 503 so resolvers retry instead of caching
    absence; 404 stays reserved for an authoritative missing key.
    """


def _b64url(value: bytes) -> str:
    return base64.urlsafe_b64encode(value).rstrip(b"=").decode("ascii")


class JWKSService:
    """Serves per-kid JWKs from the configured signing Secrets.

    Thread-safe; refreshes the backing Secrets at most once per cache TTL
    (first use is lazy — Kubernetes may not be reachable at import time).
    """

    def __init__(
        self,
        *,
        secret_names: list[str],
        reader: SecretReader,
        cache_ttl: float = DEFAULT_CACHE_TTL_SECONDS,
        max_lookups: int = DEFAULT_MAX_LOOKUPS,
        window_seconds: float = DEFAULT_WINDOW_SECONDS,
        clock: Callable[[], float] = time.monotonic,
        sidecar_reader: Optional[SidecarKeyReader] = None,
    ) -> None:
        self._secret_names = list(secret_names)
        self._reader = reader
        self._cache_ttl = cache_ttl
        self._max_lookups = max_lookups
        self._window_seconds = window_seconds
        self._clock = clock
        self._sidecar_reader = sidecar_reader

        self._lock = threading.Lock()
        self._keys: dict[str, dict] = {}
        self._refreshed_at: float = float("-inf")
        # Two separate bounds: the caller throttle (every request, anonymous
        # endpoint) and the backend budget (only work that actually reaches a
        # Secret read or a workload lookup).
        self._throttle_times: list[float] = []
        self._max_throttle = max_lookups * 10
        self._lookup_times: list[float] = []
        # Sidecar (per-sandbox) lookups: one bounded per-kid TTL cache so a
        # hot resolver does not hammer the workload read path, while memory
        # stays capped across unbounded sandbox churn.
        self._sidecar_cache: dict[str, tuple[Optional[dict], float]] = {}
        self._sidecar_cache_cap = 4096
        # The shared lock guards metadata only. Backend reads (Secret
        # refresh, workload lookup) run outside it, serialized by these
        # single-flight locks, so one slow backend read never blocks an
        # unrelated cache hit.
        self._refresh_flight = threading.Lock()
        self._sidecar_flights: dict[str, threading.Lock] = {}

    def set_sidecar_reader(self, reader: SidecarKeyReader) -> None:
        """Install the per-sandbox source (wired from the sandbox service)."""
        with self._lock:
            self._sidecar_reader = reader
            self._sidecar_cache.clear()

    @property
    def configured_secret_count(self) -> int:
        return len(self._secret_names)

    def get(self, kid: str) -> Optional[dict]:
        """Return the JWK for kid, or None when authoritatively unknown.

        Raises :class:`JWKSBackendError` for transient backend failures —
        the route maps those to 503 (retryable) so resolvers never cache a
        false absence; None (→ 404) is reserved for a genuinely missing key.
        Cache hits cost nothing and never touch the backend budget; every
        request consumes the anonymous throttle. Raises
        :class:`RateLimited` when either bound is exhausted. Off-grammar
        kids return None without reading anything. Lookup order:
        fast-sandbox signing Secrets first, then the sidecar source.
        """
        if not KID_GRAMMAR.match(kid):
            return None
        now = self._clock()
        with self._lock:
            self._consume_throttle(now)
            needs_refresh = now - self._refreshed_at > self._cache_ttl
        if needs_refresh:
            self._refresh(now)
        with self._lock:
            if kid in self._keys:
                return self._keys[kid]
            cached = self._sidecar_cache.get(kid)
            if cached is not None and now - cached[1] <= self._cache_ttl:
                return cached[0]
        return self._sidecar_lookup(kid, now)

    def _refresh(self, now: float) -> None:
        """Refresh the static signing Secrets, coalesced to one flight.

        The backend read runs outside the metadata lock; duplicate callers
        wait on the flight lock and recheck, so a slow Secret read never
        blocks unrelated cache hits. A failed refresh keeps the previously
        cached keys serving (stale-while-error) and retries next TTL —
        never a false 404.
        """
        with self._refresh_flight:
            with self._lock:
                if now - self._refreshed_at <= self._cache_ttl:
                    return  # another caller refreshed while we waited
                self._consume_budget(now)
            try:
                keys = self._read_secrets()
            except JWKSBackendError:
                logger.warning(
                    "jwks: signing Secret refresh failed; serving cached keys",
                    exc_info=True,
                )
                with self._lock:
                    self._refreshed_at = now  # retry next TTL window
                return
            with self._lock:
                self._keys = keys
                self._refreshed_at = now

    def _sidecar_lookup(self, kid: str, now: float) -> Optional[dict]:
        with self._lock:
            flight = self._sidecar_flights.setdefault(kid, threading.Lock())
        try:
            with flight:
                with self._lock:
                    cached = self._sidecar_cache.get(kid)
                    if cached is not None and now - cached[1] <= self._cache_ttl:
                        return cached[0]
                    self._consume_budget(now)
                    reader = self._sidecar_reader
                if reader is None:
                    with self._lock:
                        self._record_sidecar(kid, None, now)
                    return None
                try:
                    jwk = reader(kid)
                except JWKSBackendError:
                    raise
                except Exception as exc:
                    # Transient workload-read failure: retryable, never
                    # cached as absence.
                    raise JWKSBackendError(
                        f"sidecar key read for {kid} failed: {exc}"
                    ) from exc
                with self._lock:
                    self._record_sidecar(kid, jwk, now)
                return jwk
        finally:
            with self._lock:
                if self._sidecar_flights.get(kid) is flight:
                    del self._sidecar_flights[kid]

    def _record_sidecar(self, kid: str, jwk: Optional[dict], now: float) -> None:
        if len(self._sidecar_cache) >= self._sidecar_cache_cap:
            for stale in list(self._sidecar_cache)[: len(self._sidecar_cache) // 2]:
                del self._sidecar_cache[stale]
        self._sidecar_cache[kid] = (jwk, now)

    def _consume_budget(self, now: float) -> None:
        self._lookup_times = [
            t for t in self._lookup_times if now - t < self._window_seconds
        ]
        if len(self._lookup_times) >= self._max_lookups:
            raise RateLimited("jwks lookup budget exhausted")
        self._lookup_times.append(now)

    def _consume_throttle(self, now: float) -> None:
        """Caller-side throttle: bounds every request, hit or miss, so one
        anonymous flood cannot pin the endpoint; an order of magnitude above
        the backend budget so legitimate caches-first traffic never sees it."""
        self._throttle_times = [
            t for t in self._throttle_times if now - t < self._window_seconds
        ]
        if len(self._throttle_times) >= self._max_throttle:
            raise RateLimited("jwks request throttle exceeded")
        self._throttle_times.append(now)

    def _read_secrets(self) -> dict[str, dict]:
        keys: dict[str, dict] = {}
        for name in self._secret_names:
            try:
                data = self._reader(name)
            except Exception as exc:
                # Transient read failure: surface it (the caller keeps the
                # previously cached keys) rather than silently dropping the
                # Secret's kids — a missing key must stay authoritative.
                raise JWKSBackendError(
                    f"egress signing Secret {name} read failed: {exc}"
                ) from exc
            if not data:
                logger.warning("jwks: egress signing Secret %s not found", name)
                continue
            jwk = _secret_to_jwk(name, data)
            if jwk is None:
                continue
            kid = jwk["kid"]
            if kid in keys:
                logger.error(
                    "jwks: duplicate kid %s from Secrets — keeping the "
                    "earlier (current-version-first) entry",
                    kid,
                )
                continue
            keys[kid] = jwk
        return keys


def _secret_to_jwk(secret_name: str, data: Mapping[str, bytes]) -> Optional[dict]:
    def entry(name: str) -> Optional[bytes]:
        value = data.get(name)
        if value is None:
            return None
        return value if isinstance(value, bytes) else value.encode()

    kid_raw = entry(KID_ENTRY)
    pub_raw = entry(SIGNING_PUBLIC_KEY_ENTRY)
    if kid_raw is None or pub_raw is None:
        logger.error(
            "jwks: egress signing Secret %s must carry %s and %s entries",
            secret_name,
            SIGNING_PUBLIC_KEY_ENTRY,
            KID_ENTRY,
        )
        return None
    kid = kid_raw.decode("utf-8", errors="replace").strip()
    if not KID_GRAMMAR.match(kid):
        logger.error("jwks: egress signing Secret %s carries off-grammar kid %r", secret_name, kid)
        return None
    x = pub_raw.decode("ascii", errors="replace").strip()
    try:
        raw = base64.urlsafe_b64decode(x + "=" * (-len(x) % 4))
    except Exception:
        logger.error("jwks: egress signing Secret %s signing.pub is not base64url", secret_name)
        return None
    if len(raw) != _ED25519_PUBLIC_KEY_BYTES:
        logger.error(
            "jwks: egress signing Secret %s signing.pub is %d bytes, want %d "
            "(raw Ed25519 public key)",
            secret_name,
            len(raw),
            _ED25519_PUBLIC_KEY_BYTES,
        )
        return None
    return {
        "kty": "OKP",
        "crv": "Ed25519",
        "x": x,
        "kid": kid,
        "alg": "EdDSA",
        "use": "sig",
    }


def build_k8s_secret_reader(
    core_v1_api: object, namespace: str
) -> Callable[[str], Optional[Mapping[str, bytes]]]:
    """Adapt the CoreV1 API into a SecretReader for the platform namespace."""

    def reader(name: str) -> Optional[Mapping[str, bytes]]:
        import base64

        from kubernetes.client import ApiException

        try:
            secret = core_v1_api.read_namespaced_secret(name, namespace)  # type: ignore[attr-defined]
        except ApiException as exc:
            if exc.status == 404:
                return None
            raise
        # V1Secret.data carries base64-encoded entry values (the Kubernetes
        # wire format); the parser expects the decoded contents.
        decoded: dict[str, bytes] = {}
        for key, value in (secret.data or {}).items():
            if isinstance(value, str):
                try:
                    decoded[key] = base64.b64decode(value)
                except Exception:
                    logger.warning(
                        "jwks: egress signing Secret %s entry %s is not valid base64",
                        name,
                        key,
                    )
                    return {}
        return decoded

    return reader


def build_jwks_service_from_config(
    kubernetes_config,
    core_v1_api: object,
    sidecar_reader: Optional[SidecarKeyReader] = None,
) -> "JWKSService":
    """Wire the service from KubernetesRuntimeConfig (fast-sandbox profile)."""
    namespace = kubernetes_config.namespace or "default"
    return JWKSService(
        secret_names=kubernetes_config.egress_signing_key_secrets,
        reader=build_k8s_secret_reader(core_v1_api, namespace),
        sidecar_reader=sidecar_reader,
    )


def build_sidecar_only_jwks_service(
    sidecar_reader: SidecarKeyReader,
) -> "JWKSService":
    """Sidecar-only service for runtimes without signing Secrets (Docker)."""
    return JWKSService(
        secret_names=[],
        reader=lambda _name: None,
        sidecar_reader=sidecar_reader,
    )
