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

"""JWKS fetching with bounded caching for the resolver verification SDK.

The lifecycle server publishes one public key per kid at
``{jwks_base}/jwks/{kid}``. This client keeps a bounded positive cache (a JWK
is fetched on first use and re-validated at most once per hour — which is
what makes rotation an effective revocation: a retired kid disappears and
the next refresh rejects its credentials), a bounded negative cache for
unknown-but-well-formed kids, and a per-process fetch budget so spraying
random kids cannot amplify into lifecycle-server lookups (OSEP-0025 JWKS
Endpoint Security).

The JWKS base URL comes from operator configuration only, never from token
contents; kid substitutes into the lookup path only after passing the
platform grammar locally; and TLS verification is never disabled.
"""

from __future__ import annotations

import base64
import json
import threading
import time
from typing import Any, Callable, Mapping, Optional, Protocol

import httpx

from .verifier import KID_GRAMMAR, VerifyError, VerifierConfig, verify_credential

#: A cached key is re-validated at most this often (rotation == revocation
#: within this window).
DEFAULT_MAX_CACHE_AGE_SECONDS = 3600.0

#: A missed kid is re-fetched at most this often.
DEFAULT_NEGATIVE_TTL_SECONDS = 300.0

#: Fetch budget per window, per client (== per resolver process).
DEFAULT_MAX_FETCHES = 60
DEFAULT_WINDOW_SECONDS = 60.0


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


class JWKSClient:
    """Bounded-caching JWKS client plus a one-call verify convenience."""

    def __init__(
        self,
        config: VerifierConfig,
        *,
        transport: Optional[Transport] = None,
        now: Optional[Callable[[], float]] = None,
        max_cache_age: float = DEFAULT_MAX_CACHE_AGE_SECONDS,
        negative_ttl: float = DEFAULT_NEGATIVE_TTL_SECONDS,
        max_fetches: int = DEFAULT_MAX_FETCHES,
        window_seconds: float = DEFAULT_WINDOW_SECONDS,
        fetch_timeout: float = 5.0,
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
        self._max_fetches = max_fetches
        self._window_seconds = window_seconds
        self._fetch_timeout = fetch_timeout

        self._lock = threading.Lock()
        self._keys: dict[str, tuple[Mapping[str, Any], float]] = {}
        self._misses: dict[str, float] = {}
        self._fetch_times: list[float] = []
        self._total_fetches = 0

    # -- public API ---------------------------------------------------------

    def verify(
        self,
        credential: str,
        *,
        document_subject: str,
        now: Optional[int] = None,
    ) -> str:
        """Verify one credential, fetching its JWK as needed.

        Returns the egress-attested subject; raises :class:`VerifyError`
        (map to HTTP 401) on any failure, including first-contact fetch
        failures — known keys keep verifying offline when the server is
        unreachable.
        """
        if now is None:
            now = int(self._now())
        kid = _header_kid(credential)
        jwk = self._key_for_kid(kid)
        return verify_credential(
            credential,
            jwk=jwk,
            document_subject=document_subject,
            config=self._config,
            now=now,
        )

    @property
    def fetch_count(self) -> int:
        """Total JWKS fetches ever performed (test/observability seam)."""
        with self._lock:
            return self._total_fetches

    # -- internals ----------------------------------------------------------

    def _key_for_kid(self, kid: str) -> Optional[Mapping[str, Any]]:
        if not KID_GRAMMAR.match(kid):
            # Local rejection: no fetch, no cache pollution.
            raise VerifyError("kid_grammar", "kid does not match the platform identifier grammar")

        now = self._now()
        with self._lock:
            cached = self._keys.get(kid)
            if cached is not None:
                jwk, fetched_at = cached
                if now - fetched_at <= self._max_cache_age:
                    return jwk
                # Bounded staleness: the entry stays usable while a refresh
                # is attempted, and disappears once the refresh misses.
                refreshed = self._fetch_locked(kid, now)
                if refreshed is None:
                    del self._keys[kid]
                    self._misses[kid] = now
                    return None
                return refreshed

            missed_at = self._misses.get(kid)
            if missed_at is not None and now - missed_at <= self._negative_ttl:
                return None

            jwk = self._fetch_locked(kid, now)
            if jwk is None:
                self._misses[kid] = now
            else:
                self._misses.pop(kid, None)
            return jwk

    def _fetch_locked(self, kid: str, now: float) -> Optional[Mapping[str, Any]]:
        # Caller holds the lock; a single flight also dedupes concurrent
        # fetches for the same kid.
        self._fetch_times = [t for t in self._fetch_times if now - t < self._window_seconds]
        if len(self._fetch_times) >= self._max_fetches:
            raise VerifyError(
                "unknown_kid", "JWKS fetch budget exhausted; retry later"
            )
        url = f"{self._config.jwks_base.rstrip('/')}/jwks/{kid}"
        try:
            status, body = self._transport(url, timeout=self._fetch_timeout)
        except Exception as exc:
            # First-contact fail-closed: known keys still verify offline.
            raise VerifyError("unknown_kid", f"JWKS fetch failed: {exc}") from exc
        self._fetch_times.append(now)
        self._total_fetches += 1
        if status != 200:
            return None
        try:
            jwk = json.loads(body)
        except Exception as exc:
            raise VerifyError("unknown_kid", "JWKS response is not JSON") from exc
        if not isinstance(jwk, dict) or jwk.get("kty") != "OKP" or "x" not in jwk:
            raise VerifyError("unknown_kid", "JWKS response is not an Ed25519 JWK")
        self._keys[kid] = (jwk, now)
        return jwk


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
