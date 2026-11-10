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

"""Sidecar caller-authentication key provisioning (OSEP-0025 D5, batch 2).

At sandbox creation the server generates one Ed25519 keypair per sandbox:
the private key (PKCS8 PEM) is injected into the egress sidecar through the
OPENSANDBOX_EGRESS_TOKEN env channel and then discarded server-side; the
public JWK is written onto the workload (K8s annotation / Docker label) so
the JWKS endpoint can serve it for the life of the workload. Sandbox
recreation produces a fresh keypair — the JWKS entry is replaced with it,
which is the whole revocation story for sidecar kids.
"""

from __future__ import annotations

import base64
import json
import re
from dataclasses import dataclass
from typing import Any

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import (
    Encoding,
    NoEncryption,
    PrivateFormat,
    PublicFormat,
)

# Same pattern as the egress mint side and the resolver SDK (the vectors'
# contract block is the frozen source).
KID_GRAMMAR = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]{0,62}(:v[0-9]{1,8})?$")


@dataclass(frozen=True)
class SidecarSigningMaterial:
    """One per-sandbox keypair, split by audience."""

    kid: str
    #: PKCS8 PEM private key — destined only for the egress sidecar env.
    private_pem: str
    #: The public JWK (dict, kty/crv/x/kid/alg/use) — public material only.
    jwk: dict[str, Any]

    @property
    def jwk_json(self) -> str:
        return json.dumps(self.jwk, separators=(",", ":"), sort_keys=True)


def generate_sidecar_signing_material(sandbox_id: str) -> SidecarSigningMaterial:
    """Generate the per-sandbox keypair; kid is the sandboxId."""
    if not KID_GRAMMAR.match(sandbox_id):
        raise ValueError(
            f"sandbox id {sandbox_id!r} does not match the credential kid "
            "grammar; refusing to provision caller-auth material"
        )
    private_key = Ed25519PrivateKey.generate()
    private_pem = private_key.private_bytes(
        Encoding.PEM, PrivateFormat.PKCS8, NoEncryption()
    ).decode("ascii")
    public_raw = private_key.public_key().public_bytes(
        Encoding.Raw, PublicFormat.Raw
    )
    x = base64.urlsafe_b64encode(public_raw).rstrip(b"=").decode("ascii")
    return SidecarSigningMaterial(
        kid=sandbox_id,
        private_pem=private_pem,
        jwk={
            "kty": "OKP",
            "crv": "Ed25519",
            "x": x,
            "kid": sandbox_id,
            "alg": "EdDSA",
            "use": "sig",
        },
    )


def parse_recorded_jwk(raw: str) -> dict[str, Any] | None:
    """Decode a workload-recorded JWK (annotation/label value).

    Returns None for anything malformed — a broken record only removes that
    sandbox's JWKS entry, never the endpoint.
    """
    try:
        value = json.loads(raw)
    except (TypeError, ValueError):
        return None
    if not isinstance(value, dict) or value.get("kty") != "OKP":
        return None
    if not isinstance(value.get("kid"), str) or not isinstance(value.get("x"), str):
        return None
    return value
