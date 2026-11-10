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

"""The public entry point: CredentialVaultCallerAuth."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from opensandbox.resolver_auth import (
    CREDENTIAL_HEADER,
    CredentialVaultCallerAuth,
    VerifyError,
)

REPO_ROOT = Path(__file__).resolve().parents[4]
VECTORS = (
    REPO_ROOT
    / "components"
    / "egress"
    / "pkg"
    / "credentialvault"
    / "testdata"
    / "resolver_auth_vectors.json"
)


class FakeClock:
    def __init__(self) -> None:
        self.now = 1000.0

    def __call__(self) -> float:
        return self.now


class MapTransport:
    def __init__(self, jwks: dict) -> None:
        self.jwks = jwks
        self.calls = 0

    def __call__(self, url: str, *, timeout: float):
        self.calls += 1
        kid = url.rsplit("/", 1)[-1]
        if kid in self.jwks:
            return 200, json.dumps(self.jwks[kid]).encode()
        return 404, b"{}"


@pytest.fixture()
def vectors() -> dict:
    with VECTORS.open() as handle:
        return json.load(handle)


def _happy_parts(vectors):
    case = next(c for c in vectors["cases"] if c["name"] == "valid_sidecar")
    key = next(k for k in vectors["keys"] if k["id"] == "sidecar-1")
    import base64

    claims = json.loads(
        base64.urlsafe_b64decode(case["jwt"].split(".")[1] + "=" * (-len(case["jwt"].split(".")[1]) % 4))
    )
    return case["jwt"], claims["sub"], key["public_jwk"]


def test_verify_caller_returns_attested_subject(vectors) -> None:
    jwt, subject, jwk = _happy_parts(vectors)
    transport = MapTransport({"sbx-7f3a2c": jwk})
    auth = CredentialVaultCallerAuth(
        jwks_base="https://server.invalid/credential-vault",
        expected_audience="credential-resolver@resolver.internal",
        transport=transport,
        now=FakeClock(),
    )
    result = auth.verify_caller(jwt, document_subject=subject, now=1_790_000_600)
    assert result == subject
    assert auth.jwks_fetch_count == 1
    # The cached key serves the next call without a fetch.
    auth.verify_caller(jwt, document_subject=subject, now=1_790_000_601)
    assert auth.jwks_fetch_count == 1


def test_verify_caller_rejects_mismatched_subject(vectors) -> None:
    jwt, _, jwk = _happy_parts(vectors)
    auth = CredentialVaultCallerAuth(
        jwks_base="https://server.invalid/credential-vault",
        expected_audience="credential-resolver@resolver.internal",
        transport=MapTransport({"sbx-7f3a2c": jwk}),
        now=FakeClock(),
    )
    with pytest.raises(VerifyError) as excinfo:
        auth.verify_caller(jwt, document_subject="sbx-somebodyelse", now=1_790_000_600)
    assert excinfo.value.code == "subject_mismatch"


def test_instances_are_stateful_not_static(vectors) -> None:
    """Caches are per instance: two instances each fetch once."""
    jwt, subject, jwk = _happy_parts(vectors)
    transport = MapTransport({"sbx-7f3a2c": jwk})
    clock = FakeClock()
    first = CredentialVaultCallerAuth(
        jwks_base="https://server.invalid/credential-vault",
        expected_audience="credential-resolver@resolver.internal",
        transport=transport,
        now=clock,
    )
    second = CredentialVaultCallerAuth(
        jwks_base="https://server.invalid/credential-vault",
        expected_audience="credential-resolver@resolver.internal",
        transport=transport,
        now=clock,
    )
    first.verify_caller(jwt, document_subject=subject, now=1_790_000_600)
    second.verify_caller(jwt, document_subject=subject, now=1_790_000_600)
    assert first.jwks_fetch_count == 1
    assert second.jwks_fetch_count == 1, "caches are per instance"
    assert transport.calls == 2, "two instances each fetch once"


def test_header_constant_exported() -> None:
    assert CREDENTIAL_HEADER == "OpenSandbox-Egress-Credential"
