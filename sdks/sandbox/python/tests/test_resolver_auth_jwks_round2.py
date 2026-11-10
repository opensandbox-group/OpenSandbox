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

"""Second-round review hardening (PR #2707)."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from opensandbox.resolver_auth import VerifyError
from opensandbox.resolver_auth.jwks import JWKSClient

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
    def __init__(self, start: float = 1_000_000.0) -> None:
        self.now = start

    def __call__(self) -> float:
        return self.now

    def advance(self, seconds: float) -> None:
        self.now += seconds


def _cfg():
    from opensandbox.resolver_auth import VerifierConfig

    return VerifierConfig(
        jwks_base="https://server.invalid",
        expected_audience="credential-resolver@resolver.internal",
    )


def _sidecar_parts():
    vectors = json.loads(VECTORS.read_text())
    case = next(c for c in vectors["cases"] if c["name"] == "valid_sidecar")
    key = next(k for k in vectors["keys"] if k["id"] == "sidecar-1")
    return case["jwt"], json.loads(
        __import__("base64").urlsafe_b64decode(
            case["jwt"].split(".")[1] + "=" * (-len(case["jwt"].split(".")[1]) % 4)
        )
    )["sub"], key["public_jwk"]


def test_inflight_locks_are_reclaimed() -> None:
    """Finished flights must not accumulate locks."""

    class Always404:
        def __call__(self, url: str, *, timeout: float):
            return 404, b"{}"

    clock = FakeClock()
    client = JWKSClient(_cfg(), transport=Always404(), now=clock)
    # Every kid misses authoritatively; each completed flight must leave no
    # lock behind.
    import base64 as b64

    for i in range(100):
        kid = f"sbx-churn{i:03d}"
        header = b64.urlsafe_b64encode(
            json.dumps({"alg": "EdDSA", "typ": "JWT", "kid": kid}).encode()
        ).rstrip(b"=").decode()
        claims = b64.urlsafe_b64encode(
            json.dumps(
                {
                    "iss": "opensandbox",
                    "aud": "credential-resolver@resolver.internal",
                    "sub": kid,
                    "iat": 1_790_000_000,
                    "exp": 1_790_003_600,
                    "jti": f"reclaim{i:019d}",
                }
            ).encode()
        ).rstrip(b"=").decode()
        with pytest.raises(VerifyError):
            client.verify(f"{header}.{claims}.AAAA", document_subject=kid, now=1_790_000_600)
    assert len(client._inflight) == 0, (
        f"flight metadata must not accumulate (got {len(client._inflight)})"
    )


def test_default_now_boundary_during_fetch() -> None:
    """Deterministic boundary: a token valid exactly at fetch start and
    expired at fetch end (no explicit now) must be rejected."""
    import base64 as b64

    vectors = json.loads(VECTORS.read_text())
    key = next(k for k in vectors["keys"] if k["id"] == "sidecar-1")
    jwk = key["public_jwk"]
    kid = jwk["kid"]

    class Clock:
        # exp+29: inside the 30s leeway at fetch start.
        now = 1_790_003_629.0

        def __call__(self) -> float:
            return self.now

    clock = Clock()

    class SlowFetch:
        def __call__(self, url: str, *, timeout: float):
            clock.now += 2.0  # fetch outlasts the leeway window: exp+31
            return 200, json.dumps(jwk).encode()

    client = JWKSClient(_cfg(), transport=SlowFetch(), now=clock)
    # iat = clock start; exp = iat + 3600. At fetch end the token is 1s
    # past exp with zero leeway margin left.
    header = b64.urlsafe_b64encode(
        json.dumps({"alg": "EdDSA", "typ": "JWT", "kid": kid}).encode()
    ).rstrip(b"=").decode()
    claims = b64.urlsafe_b64encode(
        json.dumps(
            {
                "iss": "opensandbox",
                "aud": "credential-resolver@resolver.internal",
                "sub": "sbx-latenow1",
                "iat": 1_790_000_000,
                "exp": 1_790_003_600,
                "jti": "latenow-boundary-01",
            }
        ).encode()
    ).rstrip(b"=").decode()
    signature = key and b64.urlsafe_b64encode(
        __import__("cryptography.hazmat.primitives.serialization", fromlist=["load_pem_private_key"])
        .load_pem_private_key(key["private_key_pem"].encode(), password=None)
        .sign(f"{header}.{claims}".encode())
    ).rstrip(b"=").decode()
    token = f"{header}.{claims}.{signature}"

    with pytest.raises(VerifyError) as excinfo:
        client.verify(token, document_subject="sbx-latenow1")
    assert excinfo.value.code == "expired", (
        "verification time must be read after the fetch, not before"
    )
