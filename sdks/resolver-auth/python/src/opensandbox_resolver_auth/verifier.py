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

"""Offline verification of OpenSandbox egress caller credentials (OSEP-0025).

The egress mints one Ed25519 JWT per (subject, resolver origin) and sends it
as the ``OpenSandbox-Egress-Credential`` header on every TCP resolve dial.
This module implements the resolver side: header whitelist, claim bindings,
and signature verification against a JWK supplied by :mod:`.jwks` (or
directly by the caller). The wire contract is frozen in
``components/egress/pkg/credentialvault/testdata/resolver_auth_vectors.json``
and the test suite walks that same file, so Go and Python cannot drift.
"""

from __future__ import annotations

import base64
import json
import re
from dataclasses import dataclass
from typing import Any, Mapping, Optional

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

#: The single header carrying the compact JWT.
CREDENTIAL_HEADER = "OpenSandbox-Egress-Credential"

#: The platform issuer; matched against the JWT ``iss`` claim.
DEFAULT_ISSUER = "opensandbox"

#: The only legal algorithm; anything else is rejected with no negotiation.
ALG = "EdDSA"

#: ``typ`` header, when present, must be exactly this.
TYP = "JWT"

#: Verification leeway in seconds (``now <= exp + leeway``; ``iat`` may be at
#: most this far in the future).
LEEWAY_SECONDS = 30

#: Maximum ``exp - iat`` window in seconds.
TTL_MAX_SECONDS = 3600

#: Header parameters allowed at all; ``jku``/``x5u``/``jwk`` are rejected on
#: sight (key-injection vectors).
_ALLOWED_HEADER_PARAMS = frozenset({"alg", "kid", "typ"})

#: Platform identifier grammar: a sidecar sandboxId or a fast-sandbox
#: ``<pool>:v<version>`` kid. Off-grammar kids are rejected locally, before
#: any JWKS fetch.
KID_GRAMMAR = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]{0,62}(:v[0-9]{1,8})?$")


class VerifyError(Exception):
    """A credential failed verification; ``code`` is the stable reason.

    Codes match the ``errors`` enum in the shared vectors file, e.g.
    ``alg_pinned``, ``typ_invalid``, ``kid_grammar``, ``kid_missing``,
    ``header_parameter``, ``signature``, ``issuer``, ``audience``,
    ``subject_mismatch``, ``expired``, ``iat_future``, ``window``,
    ``unknown_kid``. Map every failure to HTTP 401.
    """

    def __init__(self, code: str, detail: str = "") -> None:
        super().__init__(f"{code}: {detail}" if detail else code)
        self.code = code
        self.detail = detail


@dataclass(frozen=True)
class VerifierConfig:
    """The three resolver-side configuration values.

    ``jwks_base`` is only consumed by :mod:`.jwks`; the offline verifier in
    this module takes the JWK directly.
    """

    jwks_base: str
    expected_issuer: str = DEFAULT_ISSUER
    #: The resolver's own origin — the identical string the egress mints as
    #: ``aud`` (web origin of its configured endpoint, path prefix excluded).
    expected_audience: str = ""


def _b64url_decode(segment: str) -> bytes:
    try:
        return base64.urlsafe_b64decode(segment + "=" * (-len(segment) % 4))
    except Exception as exc:  # invalid base64 anywhere is a signature failure
        raise VerifyError("signature", "segment is not base64url") from exc


def _decode_json(raw: bytes, code: str, what: str) -> dict[str, Any]:
    try:
        value = json.loads(raw)
    except Exception as exc:
        raise VerifyError(code, f"{what} is not JSON") from exc
    if not isinstance(value, dict):
        raise VerifyError(code, f"{what} is not a JSON object")
    return value


def _jwk_public_key(jwk: Mapping[str, Any]) -> Ed25519PublicKey:
    if jwk.get("kty") != "OKP" or jwk.get("crv") != "Ed25519":
        raise VerifyError("unknown_kid", "JWK is not an Ed25519 OKP key")
    try:
        raw = _b64url_decode(str(jwk["x"]))
    except KeyError as exc:
        raise VerifyError("unknown_kid", "JWK missing x") from exc
    try:
        return Ed25519PublicKey.from_public_bytes(raw)
    except Exception as exc:
        raise VerifyError("unknown_kid", "JWK x is not a valid Ed25519 key") from exc


def verify_credential(
    credential: str,
    *,
    jwk: Optional[Mapping[str, Any]],
    document_subject: str,
    config: VerifierConfig,
    now: int,
) -> str:
    """Verify one ``OpenSandbox-Egress-Credential`` value.

    Returns the egress-attested subject (JWT ``sub``) — the sole
    authoritative identity for per-sandbox authorization, never the body
    value alone. Raises :class:`VerifyError` on any failure; callers map
    every failure to HTTP 401.

    ``jwk`` is the JWK for the token's ``kid`` (typically from
    :class:`.jwks.JWKSClient`); ``None`` means unknown kid.
    """
    if not isinstance(credential, str):
        raise VerifyError("signature", "credential is not a string")
    parts = credential.split(".")
    if len(parts) != 3 or not all(parts):
        raise VerifyError("signature", "not a three-segment compact JWS")

    header = _decode_json(_b64url_decode(parts[0]), "signature", "header")

    # Algorithm is pinned before any cryptographic work; there is no
    # negotiation and no confusion path (none, HS256, ...).
    if header.get("alg") != ALG:
        raise VerifyError("alg_pinned", f"alg {header.get('alg')!r} is not {ALG}")
    typ = header.get("typ")
    if typ is not None and typ != TYP:
        raise VerifyError("typ_invalid", f"typ {typ!r} is not {TYP}")

    kid = header.get("kid")
    if not isinstance(kid, str) or not kid:
        raise VerifyError("kid_missing", "header carries no kid")
    if not KID_GRAMMAR.match(kid):
        # Rejected locally: no JWKS fetch, no negative-cache pollution.
        raise VerifyError("kid_grammar", "kid does not match the platform identifier grammar")

    unknown = set(header) - _ALLOWED_HEADER_PARAMS
    if unknown:
        # jku/x5u/jwk land here too — keys come from the configured endpoint
        # and nowhere else.
        raise VerifyError("header_parameter", f"unexpected header parameters: {sorted(unknown)}")

    if jwk is None:
        raise VerifyError("unknown_kid", f"kid {kid!r} absent from JWKS")

    claims = _decode_json(_b64url_decode(parts[1]), "signature", "payload")

    signature = _b64url_decode(parts[2])
    key = _jwk_public_key(jwk)
    try:
        key.verify(signature, f"{parts[0]}.{parts[1]}".encode("ascii"))
    except InvalidSignature as exc:
        raise VerifyError("signature", "Ed25519 verification failed") from exc
    except Exception as exc:  # non-ascii segments and similar malformations
        raise VerifyError("signature", "credential is not well-formed") from exc

    if claims.get("iss") != config.expected_issuer:
        raise VerifyError("issuer", f"iss {claims.get('iss')!r} is not {config.expected_issuer!r}")
    if claims.get("aud") != config.expected_audience:
        raise VerifyError(
            "audience", f"aud {claims.get('aud')!r} is not this resolver's origin"
        )

    iat = claims.get("iat")
    exp = claims.get("exp")
    if not isinstance(iat, int) or not isinstance(exp, int):
        raise VerifyError("window", "iat/exp must be integer seconds")
    if exp - iat <= 0 or exp - iat > TTL_MAX_SECONDS:
        raise VerifyError("window", f"exp-iat must be in (0, {TTL_MAX_SECONDS}]")
    if now > exp + LEEWAY_SECONDS:
        raise VerifyError("expired", f"now {now} > exp {exp} + {LEEWAY_SECONDS}s")
    if iat > now + LEEWAY_SECONDS:
        raise VerifyError("iat_future", f"iat {iat} is more than {LEEWAY_SECONDS}s in the future")

    # Unknown payload claims are ignored (deliberate; see OSEP-0025's
    # "deliberate omissions" — no nbf, no scope/role claims, jti is not a
    # replay defense).

    sub = claims.get("sub")
    if not isinstance(sub, str) or not sub:
        raise VerifyError("subject_mismatch", "sub claim missing")
    if sub != document_subject:
        # The attested sub is the sole authoritative identity: on mismatch
        # the request is rejected, never silently re-keyed.
        raise VerifyError(
            "subject_mismatch", "JWT sub does not equal the resolve document subject"
        )
    return sub
