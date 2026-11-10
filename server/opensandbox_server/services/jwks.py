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

"""JWKS publication for egress caller-authentication keys (OSEP-0025 D5).

The lifecycle server publishes one public Ed25519 key per fast-sandbox pool
kid at ``GET /jwks/{kid}``. The endpoint is public by design — it serves
public keys only, exactly like an OIDC provider's ``/.well-known/jwks.json``
— and relies on kid-grammar rejection plus rate limiting, never on resolver
registration (which would reintroduce the static credential this design
eliminates).

Key material comes from the operator-provisioned signing Secrets referenced
by ``kubernetes.egress_signing_key_secrets`` (current version first; the
previous version stays listed through the rotation grace window). Each
Secret carries ``signing.pub`` (raw base64url Ed25519 public key) and ``kid``
(``<pool>:v<version>``). Reads are cached for a short TTL to absorb
first-contact bursts; a misconfigured Secret only removes its own kid and is
logged, never fatal to the others.
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
    """Returns the per-sandbox JWK recorded on a workload, or None."""

    def __call__(self, sandbox_id: str) -> Optional[dict]: ...


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
        self._lookup_times: list[float] = []
        # Sidecar (per-sandbox) lookups: one bounded per-kid TTL cache so a
        # hot resolver does not hammer the workload read path, while memory
        # stays capped across unbounded sandbox churn.
        self._sidecar_cache: dict[str, tuple[Optional[dict], float]] = {}
        self._sidecar_cache_cap = 4096

    def set_sidecar_reader(self, reader: SidecarKeyReader) -> None:
        """Install the per-sandbox source (wired from the sandbox service)."""
        with self._lock:
            self._sidecar_reader = reader
            self._sidecar_cache.clear()

    @property
    def configured_secret_count(self) -> int:
        return len(self._secret_names)

    def get(self, kid: str) -> Optional[dict]:
        """Return the JWK for kid, or None when unknown.

        Raises :class:`RateLimited` when the lookup budget is exhausted.
        Off-grammar kids return None without reading anything. Lookup order:
        fast-sandbox signing Secrets first, then the sidecar (per-sandbox)
        source.
        """
        if not KID_GRAMMAR.match(kid):
            return None
        now = self._clock()
        with self._lock:
            self._consume_budget(now)
            if now - self._refreshed_at > self._cache_ttl:
                self._keys = self._refresh_locked()
                self._refreshed_at = now
            if kid in self._keys:
                return self._keys[kid]
            return self._sidecar_locked(kid, now)

    def _sidecar_locked(self, kid: str, now: float) -> Optional[dict]:
        reader = self._sidecar_reader
        if reader is None:
            return None
        cached = self._sidecar_cache.get(kid)
        if cached is not None and now - cached[1] <= self._cache_ttl:
            return cached[0]
        try:
            jwk = reader(kid)
        except Exception:
            logger.warning(
                "jwks: sidecar key read for %s failed; treated as unknown",
                kid,
                exc_info=True,
            )
            jwk = None
        if len(self._sidecar_cache) >= self._sidecar_cache_cap:
            # Bounded memory under sandbox churn: drop the oldest half
            # (dict preserves insertion order; good enough for a cache).
            for stale in list(self._sidecar_cache)[: len(self._sidecar_cache) // 2]:
                del self._sidecar_cache[stale]
        self._sidecar_cache[kid] = (jwk, now)
        return jwk

    def _consume_budget(self, now: float) -> None:
        self._lookup_times = [
            t for t in self._lookup_times if now - t < self._window_seconds
        ]
        if len(self._lookup_times) >= self._max_lookups:
            raise RateLimited("jwks lookup budget exhausted")
        self._lookup_times.append(now)

    def _refresh_locked(self) -> dict[str, dict]:
        keys: dict[str, dict] = {}
        for name in self._secret_names:
            try:
                data = self._reader(name)
            except Exception:
                logger.warning(
                    "jwks: reading egress signing Secret %s failed; its kid "
                    "stays unpublished until the next refresh",
                    name,
                    exc_info=True,
                )
                continue
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
        from kubernetes.client import ApiException

        try:
            secret = core_v1_api.read_namespaced_secret(name, namespace)  # type: ignore[attr-defined]
        except ApiException as exc:
            if exc.status == 404:
                return None
            raise
        return dict(secret.data or {})

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
