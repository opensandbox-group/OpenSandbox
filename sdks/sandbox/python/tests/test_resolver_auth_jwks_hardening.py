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

"""Review-driven JWKS client hardening: lock discipline, transient-error
semantics, and cache eviction."""

from __future__ import annotations

import json
import threading
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


class MapTransport:
    def __init__(self, jwks: dict) -> None:
        self.jwks = jwks
        self.calls: list[str] = []

    def __call__(self, url: str, *, timeout: float):
        self.calls.append(url)
        kid = url.rsplit("/", 1)[-1]
        if kid in self.jwks:
            return 200, json.dumps(self.jwws_entry(kid)).encode()
        return 404, b"{}"

    def jwws_entry(self, kid):  # pragma: no cover - overridden below
        raise NotImplementedError


@pytest.fixture()
def vectors() -> dict:
    with VECTORS.open() as handle:
        return json.load(handle)


def make_client(transport, clock, **kwargs) -> JWKSClient:
    return JWKSClient(
        type("Cfg", (), {})() if False else _cfg(),
        transport=transport,
        now=clock,
        **kwargs,
    )


def _cfg():
    from opensandbox.resolver_auth import VerifierConfig

    return VerifierConfig(
        jwks_base="https://server.invalid",
        expected_audience="credential-resolver@resolver.internal",
    )



def test_transient_status_is_not_negative_cached(vectors) -> None:
    """A 429/503 blip must not become a five-minute unknown-kid outage."""
    case = next(c for c in vectors["cases"] if c["name"] == "valid_sidecar")
    key = next(k for k in vectors["keys"] if k["id"] == "sidecar-1")
    jwk = key["public_jwk"]

    class Flaky:
        def __init__(self) -> None:
            self.served = False

        def __call__(self, url: str, *, timeout: float):
            if not self.served:
                self.served = True
                return 429, b"{}"
            return 200, json.dumps(jwk).encode()

    clock = FakeClock()
    client = make_client(Flaky(), clock)

    with pytest.raises(VerifyError) as blip:
        client.verify(case["jwt"], document_subject="sbx-7f3a2c", now=1_790_000_600)
    assert "HTTP 429" in blip.value.detail

    # The next call retries immediately and succeeds — no negative cache.
    subject = client.verify(case["jwt"], document_subject="sbx-7f3a2c", now=1_790_000_601)
    assert subject == "sbx-7f3a2c"


def test_transient_error_keeps_stale_key_semantics(vectors) -> None:
    """Refresh failure on an expired cached key raises (retry next call);
    it must not delete the entry or cache absence."""
    case = next(c for c in vectors["cases"] if c["name"] == "valid_sidecar")
    key = next(k for k in vectors["keys"] if k["id"] == "sidecar-1")
    jwk = key["public_jwk"]

    class OneShot:
        def __init__(self) -> None:
            self.calls = 0

        def __call__(self, url: str, *, timeout: float):
            self.calls += 1
            if self.calls == 1:
                return 200, json.dumps(jwk).encode()
            raise OSError("upstream gone")

    clock = FakeClock()
    client = make_client(OneShot(), clock)
    client.verify(case["jwt"], document_subject="sbx-7f3a2c", now=1_790_000_600)

    clock.advance(3700.0)  # cached key now stale
    with pytest.raises(VerifyError) as excinfo:
        client.verify(case["jwt"], document_subject="sbx-7f3a2c", now=1_790_004_300)
    assert "fetch failed" in excinfo.value.detail


def test_slow_fetch_does_not_block_cached_key(vectors) -> None:
    """A cold fetch for one kid must not block verification of another
    kid's cached key — network I/O lives outside the cache lock."""
    happy = next(c for c in vectors["cases"] if c["name"] == "valid_sidecar")
    key = next(k for k in vectors["keys"] if k["id"] == "sidecar-1")
    jwk = key["public_jwk"]

    class Blocking:
        def __init__(self) -> None:
            self.gate = threading.Event()
            self.slow_started = threading.Event()

        def __call__(self, url: str, *, timeout: float):
            if "sbx-slowkid1" in url:
                self.slow_started.set()
                assert self.gate.wait(timeout=10), "slow fetch never released"
                return 404, b"{}"
            return 200, json.dumps(jwk).encode()

    transport = Blocking()
    clock = FakeClock()
    client = make_client(transport, clock)
    # Warm the cached key first.
    client.verify(happy["jwt"], document_subject="sbx-7f3a2c", now=1_790_000_600)

    fetch_thread = threading.Thread(
        target=lambda: client._key_for_kid("sbx-slowkid1"), daemon=True
    )
    fetch_thread.start()
    assert transport.slow_started.wait(timeout=5)

    done = threading.Event()

    def verify_cached() -> None:
        # Same kid re-verified from cache while the slow fetch is in flight.
        client.verify(happy["jwt"], document_subject="sbx-7f3a2c", now=1_790_000_601)
        done.set()

    checker = threading.Thread(target=verify_cached, daemon=True)
    checker.start()
    assert done.wait(timeout=2), (
        "cached-key verification blocked behind an unrelated slow fetch"
    )
    transport.gate.set()
    fetch_thread.join(timeout=5)


def test_expired_entries_are_reclaimed() -> None:
    """Negative-cache entries past their TTL are swept, not retained."""
    kid = "sbx-unknown01"

    class Always404:
        calls = 0

        def __call__(self, url: str, *, timeout: float):
            self.calls += 1
            return 404, b"{}"

    transport = Always404()
    clock = FakeClock()
    client = make_client(transport, clock)

    class UnknownKid:
        pass

    # Fabricate a token with the unknown (grammar-valid) kid.
    import base64 as b64

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
                "jti": "reclaimtest0123456789a",
            }
        ).encode()
    ).rstrip(b"=").decode()
    token = f"{header}.{claims}.AAAA"

    with pytest.raises(VerifyError):
        client.verify(token, document_subject=kid, now=1_790_000_600)
    assert len(client._misses) == 1

    # Past the TTL, a different unknown kid's fetch sweeps A's expired
    # entry (its own fresh entry stays, of course).
    kid_b = "sbx-unknown02"
    header_b = b64.urlsafe_b64encode(
        json.dumps({"alg": "EdDSA", "typ": "JWT", "kid": kid_b}).encode()
    ).rstrip(b"=").decode()
    claims_b = b64.urlsafe_b64encode(
        json.dumps(
            {
                "iss": "opensandbox",
                "aud": "credential-resolver@resolver.internal",
                "sub": kid_b,
                "iat": 1_790_000_000,
                "exp": 1_790_003_600,
                "jti": "reclaimtest0123456789b",
            }
        ).encode()
    ).rstrip(b"=").decode()
    token_b = f"{header_b}.{claims_b}.AAAA"
    clock.advance(301.0)
    with pytest.raises(VerifyError):
        client.verify(token_b, document_subject=kid_b, now=1_790_000_901)
    assert kid not in client._misses, "expired negative entry must be reclaimed"
    assert kid_b in client._misses
    assert transport.calls == 2
