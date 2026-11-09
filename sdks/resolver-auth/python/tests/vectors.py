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

"""Temporary test-vector minting (W3.1 stand-in until W0 is frozen).

The shared cross-language vectors will land in
``components/egress/pkg/credentialvault/testdata/`` (owned by A, D1–D2).
Until then these self-made vectors cover the same OSEP matrix; the
switch is a loader change in this directory, not a redesign.

``mint`` builds compact-JWS credentials with deliberately bendable
rules (``alg``, ``typ``, extra header members, extra claims, wrong
signing key) so each OSEP rejection rule gets its own vector.
"""

from __future__ import annotations

import base64
import json
from typing import Any

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

DEFAULT_ISSUER = "opensandbox"
DEFAULT_AUDIENCE = "credential-resolver@resolver.internal"
DEFAULT_SUBJECT = "sbx_01HZYEXAMPLE"
DEFAULT_KID = "pool-a:v2"
DEFAULT_NOW = 1_790_000_000
DEFAULT_TTL = 3600
DEFAULT_JTI = "kX9sT2vZ8qLm4NpR6wYb3A"


def b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def generate_signing_key() -> Ed25519PrivateKey:
    return Ed25519PrivateKey.generate()


def mint(
    private_key: Ed25519PrivateKey,
    *,
    kid: str | None = DEFAULT_KID,
    iss: str = DEFAULT_ISSUER,
    aud: str = DEFAULT_AUDIENCE,
    sub: str = DEFAULT_SUBJECT,
    iat: int = DEFAULT_NOW,
    exp: int | None = None,
    jti: str | None = DEFAULT_JTI,
    alg: str = "EdDSA",
    typ: str | None = "JWT",
    header_extra: dict[str, Any] | None = None,
    payload_extra: dict[str, Any] | None = None,
    signer: Ed25519PrivateKey | None = None,
) -> str:
    """Mint one credential; ``signer`` defaults to ``private_key``.

    Passing a different ``signer`` produces a token signed by the wrong
    key (kid/ownership confusion vectors).
    """
    header: dict[str, Any] = {"alg": alg}
    if kid is not None:
        header["kid"] = kid
    if typ is not None:
        header["typ"] = typ
    if header_extra is not None:
        header.update(header_extra)
    payload: dict[str, Any] = {
        "iss": iss,
        "aud": aud,
        "sub": sub,
        "iat": iat,
        "exp": exp if exp is not None else iat + DEFAULT_TTL,
    }
    if jti is not None:
        payload["jti"] = jti
    if payload_extra is not None:
        payload.update(payload_extra)
    header_b64 = b64url(json.dumps(header).encode())
    payload_b64 = b64url(json.dumps(payload).encode())
    signing_input = f"{header_b64}.{payload_b64}".encode("ascii")
    signature = (signer or private_key).sign(signing_input)
    return f"{header_b64}.{payload_b64}.{b64url(signature)}"


def public_jwk(private_key: Ed25519PrivateKey, kid: str = DEFAULT_KID) -> dict[str, Any]:
    """RFC 8037 JWK form of the key's public half."""
    raw = private_key.public_key().public_bytes_raw()
    return {"kty": "OKP", "crv": "Ed25519", "x": b64url(raw), "kid": kid}
