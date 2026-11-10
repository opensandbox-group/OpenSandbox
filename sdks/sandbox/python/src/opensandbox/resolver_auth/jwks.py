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

"""JWKS fetching with bounded caching.

Keys come from ``{jwks_base}/jwks/{kid}`` on the lifecycle server. A cached
key is re-validated at most hourly, which is what makes rotation a working
revocation. Unknown-but-well-formed kids get a bounded negative cache;
transient server failures (rate limit, 5xx, network) are NOT cached as
absence — they surface as fetch failures and allow an immediate retry on
the next call. The base URL comes from operator configuration only, TLS
verification is never disabled, and network I/O never runs under the cache
lock (per-kid single-flight instead), so one slow fetch cannot block
verification of unrelated cached keys.
"""

from __future__ import annotations

import base64
import json
import threading
import time
from collections.abc import Callable, Mapping
from typing import Any, Protocol

import httpx

from .verifier import KID_GRAMMAR, VerifierConfig, VerifyError, verify_credential

#: A cached key is re-validated at most this often (rotation == revocation
#: within this window).
DEFAULT_MAX_CACHE_AGE_SECONDS = 3600.0

#: A missed kid is re-fetched at most this often.
DEFAULT_NEGATIVE_TTL_SECONDS = 300.0

#: Hard cap on cached kids (both maps); the oldest half is dropped when
#: exceeded, bounding memory under sandbox churn.
DEFAULT_CACHE_CAP = 4096


class Transport(Protocol):
    """Fetches a URL; returns ``(status_code, body_bytes)``. Test seam."""

    def __call__(self, url: str, *, timeout: float) -> tuple[int, bytes]: ...


class _HTTPXTransport:
    def __init__(self) -> None:
        # verify stays at its default (enabled); this SDK never disables TLS
        # verification and exposes no option to do so.
        self._client = httpx.Client(follow_redirects=False)

    def __call__(self, url: str, *, timeout: float) -> tuple[int, bytes]:
        response = self._client.get(url, timeout=timeout)
        return response.status_code, response.content


class JWKSFetchError(Exception):
    """A transient JWKS fetch failure (rate limit, 5xx, network).

    Distinct from an authoritative unknown kid: callers may retry
    immediately, and no negative-cache entry is written.
    """


class JWKSClient:
    """Bounded-caching JWKS client plus a one-call verify convenience."""

    def __init__(
        self,
        config: VerifierConfig,
        *,
        transport: Transport | None = None,
        now: Callable[[], float] | None = None,
        max_cache_age: float = DEFAULT_MAX_CACHE_AGE_SECONDS,
        negative_ttl: float = DEFAULT_NEGATIVE_TTL_SECONDS,
        fetch_timeout: float = 5.0,
        cache_cap: int = DEFAULT_CACHE_CAP,
    ) -> None:
        if not config.jwks_base.startswith("https://"):
            raise ValueError(
                "jwks_base must be an https:// URL; TLS verification is never disabled"
            )
        self._config = config
        self._transport = transport or _HTTPXTransport()
        self._now = now or time.time
        self._max_cache_age = max_cache_age
        self._negative_ttl = negative_ttl
        self._fetch_timeout = fetch_timeout
        self._cache_cap = cache_cap

        # The cache lock guards metadata only; network I/O happens outside
        # it, under per-kid single-flight locks, so a slow fetch for one kid
        # never blocks cached-key verification for others.
        self._lock = threading.Lock()
        self._keys: dict[str, tuple[Mapping[str, Any], float]] = {}
        self._misses: dict[str, float] = {}
        self._inflight: dict[str, threading.Lock] = {}
        self._total_fetches = 0

    # -- public API ---------------------------------------------------------

    def verify(
        self,
        credential: str,
        *,
        document_subject: str,
        now: int | None = None,
    ) -> str:
        """Verify one credential, fetching its JWK as needed.

        Returns the egress-attested subject; raises :class:`VerifyError`
        (map to HTTP 401) on any failure, including first-contact fetch
        failures — known keys keep verifying offline when the server is
        unreachable. Transient fetch failures raise VerifyError with code
        ``unknown_kid`` but are never cached as absence.
        """
        kid = _header_kid(credential)
        try:
            jwk = self._key_for_kid(kid)
        except JWKSFetchError as exc:
            # Transient backend failure: deny this request (401) but leave
            # no negative-cache entry, so the next request retries.
            raise VerifyError("unknown_kid", str(exc)) from exc
        if now is None:
            # Read the clock only after key retrieval: a flight wait or a
            # cold fetch can take seconds, and verifying against a stale
            # timestamp would stretch the leeway window.
            now = int(self._now())
        return verify_credential(
            credential,
            jwk=jwk,
            document_subject=document_subject,
            config=self._config,
            now=now,
        )

    @property
    def fetch_count(self) -> int:
        """Total JWKS fetch attempts (test/observability seam)."""
        with self._lock:
            return self._total_fetches

    # -- internals ----------------------------------------------------------

    def _key_for_kid(self, kid: str) -> Mapping[str, Any] | None:
        if not KID_GRAMMAR.match(kid):
            # Local rejection: no fetch, no cache pollution.
            raise VerifyError("kid_grammar", "kid does not match the platform identifier grammar")

        # Fast path under the metadata lock only.
        with self._lock:
            cached = self._keys.get(kid)
            if cached is not None and self._now() - cached[1] <= self._max_cache_age:
                return cached[0]
            missed_at = self._misses.get(kid)
            if missed_at is not None and self._now() - missed_at <= self._negative_ttl:
                return None
            flight = self._inflight.setdefault(kid, threading.Lock())
        with flight:
            # Single-flight recheck after acquiring: another thread may have
            # completed the fetch while we waited.
            now = self._now()
            try:
                with self._lock:
                    cached = self._keys.get(kid)
                    if cached is not None and now - cached[1] <= self._max_cache_age:
                        return cached[0]
                    missed_at = self._misses.get(kid)
                    if missed_at is not None and now - missed_at <= self._negative_ttl:
                        return None
                # Network I/O outside every shared lock.
                try:
                    status, body = self._transport(
                        f"{self._config.jwks_base.rstrip('/')}/jwks/{kid}",
                        timeout=self._fetch_timeout,
                    )
                except Exception as exc:
                    with self._lock:
                        self._total_fetches += 1
                    raise JWKSFetchError(f"JWKS fetch failed: {exc}") from exc
                with self._lock:
                    self._total_fetches += 1
                if status == 200:
                    jwk = self._parse_jwk(body)
                    with self._lock:
                        self._record(self._keys, kid, (jwk, now))
                        self._misses.pop(kid, None)
                        self._sweep_locked(now)
                    return jwk
                if status == 404:
                    # Authoritative absence: safe to negative-cache.
                    with self._lock:
                        self._record(self._misses, kid, now)
                        self._keys.pop(kid, None)
                        self._sweep_locked(now)
                    return None
                # Rate limit or backend failure: transient, not absence.
                raise JWKSFetchError(f"JWKS fetch returned HTTP {status}")
            finally:
                # Reclaim the flight entry so kid churn does not accumulate
                # locks. Waiters hold this lock object and recheck the
                # caches on wake — never a duplicate concurrent fetch.
                with self._lock:
                    if self._inflight.get(kid) is flight:
                        del self._inflight[kid]

    def _sweep_locked(self, now: float) -> None:
        """Drop expired entries from both maps so inactive kids are
        reclaimed, not just capped by eviction."""
        for stale_kid, missed_at in list(self._misses.items()):
            if now - missed_at > self._negative_ttl:
                del self._misses[stale_kid]
        for stale_kid, (_jwk, fetched_at) in list(self._keys.items()):
            if now - fetched_at > self._max_cache_age:
                del self._keys[stale_kid]

    def _parse_jwk(self, body: bytes) -> Mapping[str, Any]:
        try:
            jwk = json.loads(body)
        except Exception as exc:
            raise JWKSFetchError("JWKS response is not JSON") from exc
        if not isinstance(jwk, dict) or jwk.get("kty") != "OKP" or "x" not in jwk:
            raise JWKSFetchError("JWKS response is not an Ed25519 JWK")
        return jwk

    def _record(self, store: dict, kid: str, value: Any) -> None:
        """Insert with bounded eviction: past the cap, drop the oldest half
        (dict preserves insertion order) — bounds memory under sandbox
        churn."""
        store[kid] = value
        if len(store) > self._cache_cap:
            for stale in list(store)[: len(store) // 2]:
                del store[stale]


def _header_kid(credential: str) -> str:
    """Extract the unverified kid purely to select the key (standard JOSE
    practice): every security-relevant check happens in verify_credential."""
    try:
        segment = credential.split(".")[0]
        header = json.loads(base64.urlsafe_b64decode(segment + "=" * (-len(segment) % 4)))
    except Exception as exc:
        raise VerifyError("signature", "credential header is not decodable") from exc
    kid = header.get("kid") if isinstance(header, dict) else None
    if not isinstance(kid, str) or not kid:
        raise VerifyError("kid_missing", "header carries no kid")
    return kid
