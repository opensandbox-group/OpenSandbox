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

"""Per-kid JWKS client with bounded caches and fail-closed semantics.

Implements the OSEP-0025 JWKS edge rules (OSEP lines 443–453):

- the base URL comes from operator configuration only, must be HTTPS,
  and certificate verification cannot be disabled (an optional CA bundle
  may pin a platform CA, which is verification, not its removal);
- per-kid URLs are built solely from a grammar-validated ``kid``, so a
  path escape or a token-supplied URL can never steer the fetch;
- the positive cache re-validates at most once per refresh interval —
  this bound is what makes key rotation equivalent to revocation;
- unknown, malformed, or unreachable kids land in a bounded negative
  cache behind a per-peer fetch rate limit, so random-kid spraying never
  amplifies into lifecycle-server lookups.

The fetch happens under one lock: concurrent resolves that miss the cache
for the same kid coalesce into a single upstream request (no stampede),
and cache state stays consistent without a second synchronization layer.
"""

from __future__ import annotations

import json
import ssl
import threading
import time
import urllib.error
import urllib.request
from collections.abc import Callable
from typing import Any
from urllib.parse import urlsplit

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

from .errors import UnknownSigningKeyError
from .jws import decode_base64url
from .keys import KeyProvider
from .kid import is_valid_kid

__all__ = ["JwksClient", "parse_ed25519_jwk", "MAX_RESPONSE_BYTES"]

#: Bound on a JWKS response body; a larger reply is treated as malformed.
MAX_RESPONSE_BYTES = 64 * 1024

#: ``url -> (status, body)``; ``urllib.error.HTTPError`` maps to its code.
Fetcher = Callable[[str], "tuple[int, bytes]"]


def _default_fetcher(cafile: str | None, timeout: float) -> Fetcher:
    context = (
        ssl.create_default_context(cafile=cafile)
        if cafile is not None
        else ssl.create_default_context()
    )
    opener = urllib.request.build_opener(
        urllib.request.HTTPSHandler(context=context)
    )

    def fetch(url: str) -> tuple[int, bytes]:
        request = urllib.request.Request(
            url, method="GET", headers={"Accept": "application/json"}
        )
        try:
            with opener.open(request, timeout=timeout) as response:
                return response.status, response.read(MAX_RESPONSE_BYTES + 1)
        except urllib.error.HTTPError as error:
            try:
                body = error.read(MAX_RESPONSE_BYTES + 1)
            finally:
                error.close()
            return error.code, body

    return fetch


def parse_ed25519_jwk(document: Any, expected_kid: str) -> Ed25519PublicKey:
    """Validate one RFC 8037 Ed25519 JWK; raise ``ValueError`` otherwise.

    Only ``kty``, ``crv``, ``x``, and (when present) ``kid`` are
    interpreted; other members are ignored, mirroring the token-side
    rule that unknown fields are not a rejection.
    """
    if not isinstance(document, dict):
        raise ValueError("JWK is not a JSON object")
    if document.get("kty") != "OKP" or document.get("crv") != "Ed25519":
        raise ValueError("JWK is not an Ed25519 OKP key")
    kid = document.get("kid")
    if kid is not None and kid != expected_kid:
        raise ValueError("JWK kid does not match the requested kid")
    x = document.get("x")
    if not isinstance(x, str) or not x:
        raise ValueError("JWK is missing the public key value")
    try:
        raw = decode_base64url(x)
    except ValueError as error:
        raise ValueError("JWK public key value is not base64url") from error
    if len(raw) != 32:
        raise ValueError("Ed25519 public key must be 32 bytes")
    return Ed25519PublicKey.from_public_bytes(raw)


class JwksClient(KeyProvider):
    """Caching per-kid JWKS fetcher; thread-safe and fail-closed."""

    def __init__(
        self,
        *,
        jwks_base: str,
        cafile: str | None = None,
        timeout: float = 5.0,
        refresh_interval: float = 3600.0,
        positive_cache_size: int = 1024,
        negative_ttl: float = 60.0,
        negative_cache_size: int = 1024,
        fetch_rate: int = 30,
        rate_window: float = 10.0,
        clock: Callable[[], float] = time.monotonic,
        fetcher: Fetcher | None = None,
    ) -> None:
        split = urlsplit(jwks_base)
        if split.scheme != "https" or not split.netloc:
            raise ValueError(
                "jwks_base must be an https:// URL (certificate verification "
                "cannot be disabled)"
            )
        if jwks_base != jwks_base.rstrip("/"):
            raise ValueError("jwks_base must not end with '/'")
        if refresh_interval <= 0 or negative_ttl <= 0:
            raise ValueError("cache intervals must be positive")
        if positive_cache_size <= 0 or negative_cache_size <= 0:
            raise ValueError("cache sizes must be positive")
        if fetch_rate <= 0 or rate_window <= 0:
            raise ValueError("fetch rate limits must be positive")
        self._base = jwks_base
        self._refresh_interval = refresh_interval
        self._positive_cache_size = positive_cache_size
        self._negative_ttl = negative_ttl
        self._negative_cache_size = negative_cache_size
        self._fetch_rate = fetch_rate
        self._rate_window = rate_window
        self._clock = clock
        self._fetcher = fetcher if fetcher is not None else _default_fetcher(cafile, timeout)
        self._lock = threading.Lock()
        self._positive: dict[str, tuple[Ed25519PublicKey, float]] = {}
        self._negative: dict[str, float] = {}
        self._fetch_times: list[float] = []

    def _url_for(self, kid: str) -> str:
        # Built from the configured base and a grammar-validated kid only.
        return f"{self._base}/jwks/{kid}"

    def _admit_fetch(self, now: float, kid: str) -> None:
        window = self._rate_window
        self._fetch_times = [t for t in self._fetch_times if now - t < window]
        if len(self._fetch_times) >= self._fetch_rate:
            raise UnknownSigningKeyError(f"{kid}: fetch rate limited")
        self._fetch_times.append(now)

    def _remember_negative(self, kid: str, now: float) -> None:
        self._positive.pop(kid, None)
        self._negative[kid] = now + self._negative_ttl
        while len(self._negative) > self._negative_cache_size:
            del self._negative[next(iter(self._negative))]

    def get(self, kid: str) -> Ed25519PublicKey:
        """Return the Ed25519 public key for ``kid`` or fail closed.

        Raises :class:`UnknownSigningKeyError` for off-grammar kids (no
        fetch), rate-limited fetches, absent keys, malformed JWKs, and
        unreachable endpoints alike — the resolver denies either way.
        """
        if not is_valid_kid(kid):
            raise UnknownSigningKeyError(f"{kid}: off-grammar kid rejected locally")
        with self._lock:
            now = self._clock()
            cached = self._positive.get(kid)
            if cached is not None and now - cached[1] < self._refresh_interval:
                return cached[0]
            denied_until = self._negative.get(kid)
            if denied_until is not None and now < denied_until:
                raise UnknownSigningKeyError(f"{kid}: negative cache hit")
            self._admit_fetch(now, kid)
            key: Ed25519PublicKey | None = None
            try:
                status, body = self._fetcher(self._url_for(kid))
                if status == 200:
                    key = parse_ed25519_jwk(json.loads(body), kid)
            except Exception:
                key = None
            if key is None:
                self._remember_negative(kid, now)
                raise UnknownSigningKeyError(f"{kid}: no verifiable key")
            while len(self._positive) >= self._positive_cache_size:
                del self._positive[next(iter(self._positive))]
            self._positive[kid] = (key, now)
            self._negative.pop(kid, None)
            return key
