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

"""OpenSandbox resolver caller-authentication SDK (OSEP-0025).

Resolver-side offline verification of the egress caller credential:
Ed25519 JWTs validated against a server-published JWKS. Integration is
three configuration values plus one ``verify`` call::

    from opensandbox_resolver_auth import (
        CredentialVerifier, JwksClient, VerificationError,
    )

    verifier = CredentialVerifier(
        key_provider=JwksClient(jwks_base="https://lifecycle-server.example"),
        expected_issuer="opensandbox",
        expected_aud="credential-resolver@resolver.internal",
    )

    try:
        verified = verifier.verify(header_value, subject=document["subject"])
    except VerificationError as error:
        return 401, {"error": error.reason}
"""

from __future__ import annotations

from importlib.metadata import PackageNotFoundError, version

from .errors import (
    CREDENTIAL_INVALID,
    CREDENTIAL_MISSING,
    SUBJECT_MISMATCH,
    UnknownSigningKeyError,
    VerificationError,
)
from .jwks import JwksClient, parse_ed25519_jwk
from .keys import KeyProvider, StaticKeyProvider
from .kid import is_valid_kid
from .verifier import CredentialVerifier, VerifiedCredential

__all__ = [
    "CREDENTIAL_MISSING",
    "CREDENTIAL_INVALID",
    "SUBJECT_MISMATCH",
    "CredentialVerifier",
    "VerifiedCredential",
    "JwksClient",
    "parse_ed25519_jwk",
    "is_valid_kid",
    "KeyProvider",
    "StaticKeyProvider",
    "VerificationError",
    "UnknownSigningKeyError",
]

try:
    __version__ = version("opensandbox-resolver-auth")
except PackageNotFoundError:  # pragma: no cover - running from a checkout
    __version__ = "0.0.0"
