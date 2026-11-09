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

"""OSEP-0025 D5: JWKS publication — service behavior and the public route.

Public-keys-only posture (the OIDC jwks.json stance): kid grammar rejects
locally, the lookup budget bounds abuse (429), reads are cached, one broken
Secret only removes its own kid, and the endpoint requires no API key.
"""

from __future__ import annotations

import base64
import json
from pathlib import Path

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from opensandbox_server.api.jwks import router as jwks_router
from opensandbox_server.middleware.auth import AuthMiddleware
from opensandbox_server.services.jwks import JWKSService, RateLimited

REPO_ROOT = Path(__file__).resolve().parents[1].parent
VECTORS_PATH = (
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


class FakeReader:
    def __init__(self, secrets: dict[str, dict[str, bytes]]) -> None:
        self.secrets = secrets
        self.calls: list[str] = []

    def __call__(self, name: str):
        self.calls.append(name)
        return self.secrets.get(name)


def vectors_sidecar() -> tuple[str, str]:
    """(kid, base64url public key x) from the shared vectors."""
    vectors = json.loads(VECTORS_PATH.read_text())
    key = next(k for k in vectors["keys"] if k["id"] == "sidecar-1")
    return key["public_jwk"]["kid"], key["public_jwk"]["x"]


def make_service(
    secrets: dict[str, dict[str, bytes]] | None = None,
    reader: FakeReader | None = None,
    **kwargs,
) -> tuple[JWKSService, FakeReader, FakeClock]:
    clock = FakeClock()
    fake = reader or FakeReader(secrets or {})
    service = JWKSService(
        secret_names=list(fake.secrets),
        reader=fake,
        clock=clock,
        **kwargs,
    )
    return service, fake, clock


class TestJWKSService:
    def test_serves_jwk_from_secret(self) -> None:
        kid, x = vectors_sidecar()
        service, reader, _ = make_service(
            {"opensandbox-egress-signing-pool-a-v2": {
                "signing.pub": x.encode(),
                "kid": kid.encode(),
            }}
        )
        jwk = service.get(kid)
        assert jwk == {
            "kty": "OKP", "crv": "Ed25519", "x": x, "kid": kid,
            "alg": "EdDSA", "use": "sig",
        }
        assert base64.urlsafe_b64decode(x + "=" * (-len(x) % 4)) is not None

    def test_unknown_kid_returns_none(self) -> None:
        kid, x = vectors_sidecar()
        service, _, _ = make_service(
            {"s": {"signing.pub": x.encode(), "kid": kid.encode()}}
        )
        assert service.get("pool-a:v9") is None

    def test_off_grammar_kid_never_reads(self) -> None:
        service, reader, _ = make_service({})
        assert service.get("../escape") is None
        assert service.get("SBX-UPPER") is None
        assert service.get("a:b:c") is None
        assert reader.calls == []

    def test_cache_prevents_repeated_reads(self) -> None:
        kid, x = vectors_sidecar()
        service, reader, clock = make_service(
            {"s": {"signing.pub": x.encode(), "kid": kid.encode()}}
        )
        service.get(kid)
        service.get(kid)
        assert reader.calls == ["s"], "cache TTL must absorb repeat lookups"

        clock.now += 46.0  # past the default 45s TTL
        service.get(kid)
        assert reader.calls == ["s", "s"]

    def test_broken_secret_only_removes_its_own_kid(self) -> None:
        kid, x = vectors_sidecar()
        service, _, _ = make_service(
            {
                "good": {"signing.pub": x.encode(), "kid": kid.encode()},
                "bad-missing-pub": {"kid": b"pool-b:v1"},
                "bad-short-key": {"signing.pub": b"AAAA", "kid": b"pool-c:v1"},
                "bad-off-grammar-kid": {"signing.pub": x.encode(), "kid": b"../x"},
                "bad-missing-kid": {"signing.pub": x.encode()},
            }
        )
        assert service.get(kid) is not None
        assert service.get("pool-b:v1") is None
        assert service.get("pool-c:v1") is None

    def test_absent_secret_is_unknown_kid(self) -> None:
        kid, x = vectors_sidecar()
        service, _, _ = make_service(
            {"ghost": {"signing.pub": x.encode(), "kid": kid.encode()}}
        )
        service._secret_names = ["ghost"]
        service._reader = lambda name: None
        assert service.get(kid) is None

    def test_duplicate_kid_keeps_first_listed(self) -> None:
        kid, x = vectors_sidecar()
        other = base64.urlsafe_b64encode(b"\x01" * 32).rstrip(b"=").decode()
        service, _, _ = make_service(
            {
                "current": {"signing.pub": x.encode(), "kid": kid.encode()},
                "stale": {"signing.pub": other.encode(), "kid": kid.encode()},
            }
        )
        assert service.get(kid)["x"] == x, "current-version-first wins"

    def test_lookup_budget_exhaustion(self) -> None:
        kid, x = vectors_sidecar()
        service, _, clock = make_service(
            {"s": {"signing.pub": x.encode(), "kid": kid.encode()}},
            max_lookups=3,
            window_seconds=60.0,
        )
        for _ in range(3):
            service.get(kid)
        with pytest.raises(RateLimited):
            service.get(kid)
        clock.now += 61.0
        assert service.get(kid) is not None  # window slides, budget recovers


class TestJWKSRoute:
    def make_app(self, service: JWKSService | None = None) -> TestClient:
        app = FastAPI()
        app.include_router(jwks_router)
        if service is not None:
            app.state.jwks_service = service
        return TestClient(app)

    def test_unconfigured_is_501(self) -> None:
        response = self.make_app().get("/jwks/pool-a:v2")
        assert response.status_code == 501

    def test_known_kid_200_unknown_404(self) -> None:
        kid, x = vectors_sidecar()
        service, _, _ = make_service(
            {"s": {"signing.pub": x.encode(), "kid": kid.encode()}}
        )
        client = self.make_app(service)
        ok = client.get(f"/jwks/{kid}")
        assert ok.status_code == 200
        assert ok.json() == {
            "kty": "OKP", "crv": "Ed25519", "x": x, "kid": kid,
            "alg": "EdDSA", "use": "sig",
        }
        assert "PRIVATE" not in ok.text
        assert client.get("/jwks/pool-z:v9").status_code == 404
        assert client.get("/jwks/..%2Fescape").status_code == 404

    def test_rate_limited_is_429(self) -> None:
        kid, x = vectors_sidecar()
        service, _, _ = make_service(
            {"s": {"signing.pub": x.encode(), "kid": kid.encode()}},
            max_lookups=2,
        )
        client = self.make_app(service)
        assert client.get(f"/jwks/{kid}").status_code == 200
        assert client.get(f"/jwks/{kid}").status_code == 200
        assert client.get(f"/jwks/{kid}").status_code == 429

    def test_endpoint_is_public(self) -> None:
        """No API key required (public-keys-only posture, OSEP-0025)."""
        assert "/jwks" in AuthMiddleware.EXEMPT_PATHS
        assert "/v1/jwks" in AuthMiddleware.EXEMPT_PATHS
