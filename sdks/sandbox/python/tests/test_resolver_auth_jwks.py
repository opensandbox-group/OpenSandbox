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

"""JWKS client behavior: positive/negative caching, rotation-as-revocation,
per-process fetch budget, local kid rejection, and forced HTTPS."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from opensandbox.resolver_auth import VerifierConfig, VerifyError
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


class MapTransport:
    """Serves per-kid JWKs from a mutable dict; 404 otherwise. Counts calls."""

    def __init__(self, jwks: dict) -> None:
        self.jwks = jwks
        self.calls: list[str] = []

    def __call__(self, url: str, *, timeout: float):
        self.calls.append(url)
        kid = url.rsplit("/", 1)[-1]
        if kid in self.jwks:
            return 200, json.dumps(self.jwks[kid]).encode()
        return 404, b"{}"


@pytest.fixture()
def vectors() -> dict:
    with VECTORS.open() as handle:
        return json.load(handle)


@pytest.fixture()
def sidecar_jwk(vectors) -> dict:
    key = next(k for k in vectors["keys"] if k["id"] == "sidecar-1")
    return key["public_jwk"]


@pytest.fixture()
def valid_jwt(vectors) -> str:
    case = next(c for c in vectors["cases"] if c["name"] == "valid_sidecar")
    return case["jwt"], json.loads(case["payload"])["sub"]


def make_client(config, transport, clock, **kwargs) -> JWKSClient:
    return JWKSClient(config, transport=transport, now=clock, **kwargs)


def base_config() -> VerifierConfig:
    return VerifierConfig(
        jwks_base="https://server.invalid",
        expected_audience="credential-resolver@resolver.internal",
    )


def test_positive_cache_first_use_then_reuse(vectors, sidecar_jwk, valid_jwt) -> None:
    jwt, subject = valid_jwt
    clock = FakeClock()
    transport = MapTransport({"sbx-7f3a2c": sidecar_jwk})
    client = make_client(base_config(), transport, clock)

    assert client.verify(jwt, document_subject=subject, now=1_790_000_600) == subject
    assert client.fetch_count == 1
    assert client.verify(jwt, document_subject=subject, now=1_790_000_601) == subject
    assert client.fetch_count == 1, "cached key must not re-fetch within the hour"


def test_rotation_revokes_within_refresh_window(sidecar_jwk, valid_jwt) -> None:
    jwt, subject = valid_jwt
    clock = FakeClock()
    transport = MapTransport({"sbx-7f3a2c": sidecar_jwk})
    client = make_client(base_config(), transport, clock)

    client.verify(jwt, document_subject=subject, now=1_790_000_600)
    # Key retired server-side; the cached copy keeps serving until max age…
    del transport.jwks["sbx-7f3a2c"]
    clock.advance(1800.0)
    client.verify(jwt, document_subject=subject, now=1_790_002_400)
    assert client.fetch_count == 1, "within max cache age the cached key still verifies offline"

    # …past max age the next use re-fetches, misses, and rejects the kid.
    clock.advance(1801.0)
    with pytest.raises(VerifyError) as excinfo:
        client.verify(jwt, document_subject=subject, now=1_790_004_001)
    assert excinfo.value.code == "unknown_kid"
    assert client.fetch_count == 2

    # The key reappears (re-provisioned) and the kid recovers.
    transport.jwks["sbx-7f3a2c"] = sidecar_jwk
    clock.advance(301.0)  # past the negative TTL (fake clock drives caching)
    # The credential's own claims still need a `now` inside its validity.
    client.verify(jwt, document_subject=subject, now=1_790_002_900)
    assert client.fetch_count == 3


def test_negative_cache_bounds_unknown_kid_fetches(valid_jwt, sidecar_jwk) -> None:
    jwt, _ = valid_jwt
    # Re-key the token's kid to an unknown-but-well-formed one: sign a fresh
    # header? Simpler: use the vectors' reject_unknown_kid case directly.
    with VECTORS.open() as handle:
        unknown_case = next(
            c for c in json.load(handle)["cases"] if c["name"] == "reject_unknown_kid"
        )
    clock = FakeClock()
    transport = MapTransport({"sbx-7f3a2c": sidecar_jwk})
    client = make_client(base_config(), transport, clock)

    for _ in range(3):
        with pytest.raises(VerifyError) as excinfo:
            client.verify(unknown_case["jwt"], document_subject="sbx-7f3a2c", now=1_790_000_600)
        assert excinfo.value.code == "unknown_kid"
    assert client.fetch_count == 1, "repeat unknown kids must hit the negative cache"

    clock.advance(301.0)
    with pytest.raises(VerifyError):
        client.verify(unknown_case["jwt"], document_subject="sbx-7f3a2c", now=1_790_000_901)
    assert client.fetch_count == 2, "past the negative TTL one re-fetch is allowed"


def test_off_grammar_kid_never_fetches(vectors) -> None:
    case = next(c for c in vectors["cases"] if c["name"] == "reject_kid_grammar_path")
    clock = FakeClock()
    transport = MapTransport({})
    client = make_client(base_config(), transport, clock)
    with pytest.raises(VerifyError) as excinfo:
        client.verify(case["jwt"], document_subject="sbx-7f3a2c", now=1_790_000_600)
    assert excinfo.value.code == "kid_grammar"
    assert transport.calls == [], "grammar violations must be rejected locally, no fetch"


def test_fetch_budget_exhaustion_fails_closed(sidecar_jwk) -> None:
    clock = FakeClock()
    transport = MapTransport({})
    client = make_client(
        base_config(), transport, clock, max_fetches=3, window_seconds=60.0
    )
    # Burn the budget with distinct well-formed unknown kids (each returns
    # None quietly — only verify() maps a miss to unknown_kid).
    for i in range(3):
        assert client._key_for_kid(f"sbx-burn{i:03d}") is None
    with pytest.raises(VerifyError) as excinfo:
        client._key_for_kid("sbx-over1")
    assert "budget" in excinfo.value.detail
    assert len(transport.calls) == 3

    # The window slides; the budget recovers.
    clock.advance(61.0)
    client._key_for_kid("sbx-over1")
    assert len(transport.calls) == 4


def test_http_jwks_base_rejected() -> None:
    with pytest.raises(ValueError, match="https://"):
        JWKSClient(
            VerifierConfig(jwks_base="http://server.invalid", expected_audience="a@b")
        )


def test_fetch_failure_fails_closed_first_contact(valid_jwt) -> None:
    jwt, subject = valid_jwt

    class ExplodingTransport:
        def __call__(self, url: str, *, timeout: float):
            raise OSError("server unreachable")

    clock = FakeClock()
    client = make_client(base_config(), ExplodingTransport(), clock)
    with pytest.raises(VerifyError) as excinfo:
        client.verify(jwt, document_subject=subject, now=1_790_000_600)
    assert excinfo.value.code == "unknown_kid"
    assert "fetch failed" in excinfo.value.detail
