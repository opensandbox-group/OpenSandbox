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

"""The credential-vault resolver entry point.

One instance serves one resolver: it bundles the three operator values
(JWKS base, expected issuer, this resolver's own origin) with the bounded
JWKS caching, and exposes the single call a resolver needs.
"""

from __future__ import annotations

from collections.abc import Callable

from .jwks import JWKSClient, Transport
from .verifier import DEFAULT_ISSUER, VerifierConfig

__all__ = ["CredentialVaultCallerAuth"]


class CredentialVaultCallerAuth:
    """Verifies egress caller credentials for a credential-vault resolver.

    Instantiate once per resolver process — the instance holds the JWKS
    caches — and call :meth:`verify_caller` per resolve request.
    """

    def __init__(
        self,
        *,
        jwks_base: str,
        expected_audience: str,
        expected_issuer: str = DEFAULT_ISSUER,
        transport: Transport | None = None,
        now: Callable[[], float] | None = None,
        max_cache_age: float = 3600.0,
        negative_ttl: float = 300.0,
        fetch_timeout: float = 5.0,
    ) -> None:
        self._config = VerifierConfig(
            jwks_base=jwks_base,
            expected_issuer=expected_issuer,
            expected_audience=expected_audience,
        )
        self._client = JWKSClient(
            self._config,
            transport=transport,
            now=now,
            max_cache_age=max_cache_age,
            negative_ttl=negative_ttl,
            fetch_timeout=fetch_timeout,
        )

    @property
    def config(self) -> VerifierConfig:
        return self._config

    @property
    def jwks_fetch_count(self) -> int:
        """Total JWKS fetches performed (observability/test seam)."""
        return self._client.fetch_count

    def verify_caller(
        self,
        credential: str,
        *,
        document_subject: str,
        now: int | None = None,
    ) -> str:
        """Verify one resolve call's ``OpenSandbox-Egress-Credential``.

        Returns the egress-attested subject (the JWT ``sub``) — the sole
        authoritative identity for per-sandbox authorization. Raises
        :class:`VerifyError` on any failure; map every failure to HTTP 401.
        Known keys keep verifying offline when the lifecycle server is
        unreachable; only first-contact fetches fail (closed).
        """
        return self._client.verify(
            credential, document_subject=document_subject, now=now
        )
