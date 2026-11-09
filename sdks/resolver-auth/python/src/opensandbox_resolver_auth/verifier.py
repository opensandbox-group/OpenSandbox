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

"""Credential verifier core (OSEP-0025 Resolver Caller Authentication).

Offline verification of the egress-minted Ed25519 JWT carried in the
``OpenSandbox-Egress-Credential`` header. Configuration is exactly three
values (OSEP line 391): the JWKS base URL (via a key provider), the
expected issuer, and the expected audience — the resolver's own origin.

Checks, in order — all of them local and network-free until the very
last step, so malformed input never triggers a JWKS fetch:

1. Header whitelist: only ``alg`` / ``kid`` / ``typ``; anything else —
   including ``jku``, ``x5u``, and embedded ``jwk`` — is rejected on
   sight (OSEP line 451). ``alg`` is pinned to ``EdDSA`` with no
   negotiation (rejects ``none`` and HMAC confusion alike); ``typ`` is
   optional but must be ``JWT`` when present. ``kid`` must match the
   platform identifier grammar or the credential is rejected locally
   with no fetch (OSEP line 450).
2. Payload bindings: ``iss`` equals the configured issuer; ``aud``
   equals the configured audience — the resolver's own origin, so a
   credential observed by one resolver cannot be replayed to another;
   ``exp - iat ≤ 3600``; ``now ≤ exp + 30``; ``iat`` no more than the
   leeway in the future.
3. Subject binding: the signed ``sub`` must equal the resolve
   document's ``subject``; on mismatch the caller is rejected, and the
   attested ``sub`` — not the body value — is the sole authoritative
   identity (OSEP lines 357, 384).
4. Signature: one offline Ed25519 verification against the key for
   ``kid``.

Deliberate omissions per OSEP line 389 — do not "helpfully" add them:
no ``nbf`` check, no ``scope``/``role``/tenant claims, and no ``jti``
dedup (one credential is legitimately reused across its whole window;
replay is bounded by ``exp`` and ``aud``). Unknown payload claims are
ignored.
"""

from __future__ import annotations

import time
from collections.abc import Callable
from dataclasses import dataclass

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

from .errors import (
    CREDENTIAL_INVALID,
    CREDENTIAL_MISSING,
    SUBJECT_MISMATCH,
    UnknownSigningKeyError,
    VerificationError,
)
from .jws import ParsedJws, parse_compact_jws
from .keys import KeyProvider
from .kid import is_valid_kid

__all__ = [
    "CredentialVerifier",
    "VerifiedCredential",
    "DEFAULT_LEEWAY_SECONDS",
    "DEFAULT_MAX_TTL_SECONDS",
]

#: Clock-skew leeway on ``exp``/``iat`` (OSEP line 385).
DEFAULT_LEEWAY_SECONDS = 30.0
#: Maximum credential window ``exp - iat`` (OSEP line 385).
DEFAULT_MAX_TTL_SECONDS = 3600.0

_ALG_EDDSA = "EdDSA"
_HEADER_FIELDS = frozenset({"alg", "kid", "typ"})


@dataclass(frozen=True)
class VerifiedCredential:
    """Claims of a verified credential; ``subject`` is authoritative."""

    kid: str
    issuer: str
    audience: str
    subject: str
    issued_at: int
    expires_at: int
    jti: str | None


class CredentialVerifier:
    """Offline verifier for ``OpenSandbox-Egress-Credential`` JWTs.

    Thread-safe when the key provider is; the verifier itself keeps no
    per-request state.
    """

    def __init__(
        self,
        *,
        key_provider: KeyProvider,
        expected_issuer: str,
        expected_aud: str,
        clock: Callable[[], float] = time.time,
        leeway: float = DEFAULT_LEEWAY_SECONDS,
        max_ttl: float = DEFAULT_MAX_TTL_SECONDS,
    ) -> None:
        if not expected_issuer or not expected_aud:
            raise ValueError("expected_issuer and expected_aud are required")
        if leeway < 0 or max_ttl <= 0:
            raise ValueError("leeway must be >= 0 and max_ttl must be positive")
        self._key_provider = key_provider
        self._expected_issuer = expected_issuer
        self._expected_aud = expected_aud
        self._clock = clock
        self._leeway = leeway
        self._max_ttl = max_ttl

    def verify(self, credential: object, *, subject: str) -> VerifiedCredential:
        """Verify one credential against the resolve document's subject.

        Raises :class:`VerificationError` with reason
        ``credential_missing`` / ``credential_invalid`` /
        ``subject_mismatch``; resolvers map every instance to 401.
        """
        if not isinstance(credential, str) or not credential:
            raise VerificationError(
                CREDENTIAL_MISSING,
                "no OpenSandbox-Egress-Credential header on the request",
            )
        parsed = parse_compact_jws(credential)
        kid = self._check_header(parsed)
        now = self._clock()
        self._check_payload(parsed, now, subject)
        self._check_signature(parsed, kid)
        payload = parsed.payload
        jti = payload.get("jti")
        return VerifiedCredential(
            kid=kid,
            issuer=payload["iss"],
            audience=payload["aud"],
            subject=payload["sub"],
            issued_at=payload["iat"],
            expires_at=payload["exp"],
            jti=jti if isinstance(jti, str) else None,
        )

    def _check_header(self, parsed: ParsedJws) -> str:
        header = parsed.header
        illegal = set(header) - _HEADER_FIELDS
        if illegal:
            raise VerificationError(
                CREDENTIAL_INVALID,
                f"illegal header parameters: {sorted(illegal)}",
            )
        if header.get("alg") != _ALG_EDDSA:
            raise VerificationError(
                CREDENTIAL_INVALID, "alg is pinned to EdDSA; no negotiation"
            )
        if "typ" in header and header["typ"] != "JWT":
            raise VerificationError(
                CREDENTIAL_INVALID, "typ must be JWT when present"
            )
        kid = header.get("kid")
        if not is_valid_kid(kid):
            raise VerificationError(
                CREDENTIAL_INVALID,
                "kid missing or off the platform identifier grammar "
                "(rejected locally, no JWKS fetch)",
            )
        return str(kid)

    def _check_payload(self, parsed: ParsedJws, now: float, subject: str) -> None:
        payload = parsed.payload
        if payload.get("iss") != self._expected_issuer:
            raise VerificationError(CREDENTIAL_INVALID, "issuer mismatch")
        if payload.get("aud") != self._expected_aud:
            raise VerificationError(
                CREDENTIAL_INVALID,
                "audience mismatch (credential not valid at this origin)",
            )
        iat = payload.get("iat")
        exp = payload.get("exp")
        if type(iat) is not int or type(exp) is not int:
            raise VerificationError(CREDENTIAL_INVALID, "iat/exp must be integers")
        if exp - iat > self._max_ttl:
            raise VerificationError(
                CREDENTIAL_INVALID, "credential window exceeds the maximum TTL"
            )
        if now > exp + self._leeway:
            raise VerificationError(CREDENTIAL_INVALID, "credential expired")
        if iat > now + self._leeway:
            raise VerificationError(CREDENTIAL_INVALID, "iat too far in the future")
        sub = payload.get("sub")
        if not isinstance(sub, str) or not sub:
            raise VerificationError(CREDENTIAL_INVALID, "sub must be a non-empty string")
        if sub != subject:
            raise VerificationError(
                SUBJECT_MISMATCH,
                "attested sub does not equal the resolve document's subject",
            )

    def _check_signature(self, parsed: ParsedJws, kid: str) -> Ed25519PublicKey:
        if len(parsed.signature) != 64:
            raise VerificationError(
                CREDENTIAL_INVALID, "EdDSA signature must be 64 bytes"
            )
        try:
            key = self._key_provider.get(kid)
        except UnknownSigningKeyError as error:
            raise VerificationError(
                CREDENTIAL_INVALID, f"no verifiable key for kid {kid}"
            ) from error
        try:
            key.verify(parsed.signature, parsed.signing_input)
        except InvalidSignature as error:
            raise VerificationError(
                CREDENTIAL_INVALID, "signature verification failed"
            ) from error
        return key
