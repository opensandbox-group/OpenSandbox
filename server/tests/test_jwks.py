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

"""JWKS publication: service behavior and the public route.

Public-keys-only posture (the OIDC jwks.json stance): kid grammar rejects
locally, the lookup budget bounds abuse (429), reads are cached, one broken
Secret only removes its own kid.
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
        # Distinct unknown kids each cost a backend lookup (sidecar miss).
        service.get("sbx-burn000")  # refresh + miss = 2 of 3
        service.get("sbx-burn001")  # miss = 3 of 3
        with pytest.raises(RateLimited):
            service.get("sbx-burn002")
        clock.now += 61.0
        assert service.get("sbx-over1") is None  # window slides, budget recovers


class TestSidecarSource:
    """Per-sandbox JWKs from the workload record, after static miss."""

    def test_sidecar_consulted_after_static_miss(self) -> None:
        kid, x = vectors_sidecar()
        material_jwk = {"kty": "OKP", "crv": "Ed25519", "x": "AAAA", "kid": "sbx-side1"}
        calls: list[str] = []

        def reader(sandbox_id: str):
            calls.append(sandbox_id)
            return material_jwk if sandbox_id == "sbx-side1" else None

        clock = FakeClock()
        service, _, _ = make_service(
            {"s": {"signing.pub": x.encode(), "kid": kid.encode()}},
            reader=None,
        )
        service._sidecar_reader = reader
        service._clock = clock

        assert service.get(kid)["x"] == x, "static pool keys still win"
        assert service.get("sbx-side1") == material_jwk, "sidecar miss falls through"
        assert service.get("sbx-other") is None
        assert calls == ["sbx-side1", "sbx-other"]

    def test_sidecar_results_cached_with_ttl(self) -> None:
        kid, x = vectors_sidecar()
        calls: list[str] = []

        def reader(_sandbox_id: str):
            calls.append("hit")
            return {"kty": "OKP", "crv": "Ed25519", "x": "AAAA", "kid": kid}

        clock = FakeClock()
        service, _, _ = make_service({})
        service._sidecar_reader = reader
        service._clock = clock

        service.get(kid)
        service.get(kid)
        assert len(calls) == 1, "sidecar lookups cache within the TTL"

        clock.now += 46.0
        service.get(kid)
        assert len(calls) == 2

    def test_sidecar_reader_error_is_unknown_kid(self) -> None:
        def exploding(_sandbox_id: str):
            raise RuntimeError("workload read failed")

        service, _, _ = make_service({})
        service._sidecar_reader = exploding
        assert service.get("sbx-side1") is None

    def test_sidecar_route_serves_jwk(self) -> None:
        kid, x = vectors_sidecar()
        material_jwk = {
            "kty": "OKP", "crv": "Ed25519",
            "x": base64.urlsafe_b64encode(b"\x02" * 32).rstrip(b"=").decode(),
            "kid": "sbx-side1", "alg": "EdDSA", "use": "sig",
        }
        service, _, _ = make_service({})
        service._sidecar_reader = lambda sid: material_jwk if sid == "sbx-side1" else None
        client = TestJWKSRoute().make_app(service)
        ok = client.get("/credential-vault/jwks/sbx-side1")
        assert ok.status_code == 200
        assert ok.json() == material_jwk
        assert client.get("/credential-vault/jwks/sbx-gone1").status_code == 404

    def test_cache_hits_do_not_consume_backend_budget(self) -> None:
        """One caller hammering unknown kids must not starve cached-key
        reads: hits and throttle are free of the backend budget."""
        kid, x = vectors_sidecar()
        service, _, _ = make_service(
            {"s": {"signing.pub": x.encode(), "kid": kid.encode()}},
            max_lookups=2,
        )
        # First read populates the static cache (one refresh, one budgeted
        # lookup already consumed by the refresh itself).
        assert service.get(kid) is not None
        # Unknown kids burn the backend budget (sidecar reader unset, so
        # each miss costs a lookup slot).
        for i in range(4):
            try:
                service.get(f"sbx-flood{i:03d}")
            except RateLimited:
                break
        # The cached key must still be readable despite the exhausted
        # backend budget.
        assert service.get(kid) is not None


class TestK8sSecretWireFormat:
    """build_k8s_secret_reader must decode V1Secret.data's base64 layer."""

    def _core_api(self, data: dict):
        class FakeSecret:
            pass

        secret = FakeSecret()
        secret.data = data

        class FakeApi:
            def read_namespaced_secret(self, name, namespace):
                return secret

        return FakeApi()

    def test_wire_format_secret_decodes(self) -> None:
        import base64 as b64

        from opensandbox_server.services.jwks import build_jwks_service_from_config

        kid = "pool-a:v2"
        raw_x = b64.urlsafe_b64encode(b"\x07" * 32).rstrip(b"=").decode()
        wire = {
            "signing.pub": b64.b64encode(raw_x.encode()).decode(),
            "kid": b64.b64encode(kid.encode()).decode(),
        }
        service = build_jwks_service_from_config(
            type("C", (), {"namespace": "ns", "egress_signing_key_secrets": ["s"]})(),
            self._core_api(wire),
        )
        jwk = service.get(kid)
        assert jwk is not None, "the Kubernetes base64 layer must be decoded"
        assert jwk["x"] == raw_x
        assert jwk["kid"] == kid

    def test_invalid_base64_entry_is_rejected(self) -> None:
        from opensandbox_server.services.jwks import build_jwks_service_from_config

        wire = {"signing.pub": "!!!not-base64!!!", "kid": "cG9vbC1hOnYy"}
        service = build_jwks_service_from_config(
            type("C", (), {"namespace": "ns", "egress_signing_key_secrets": ["s"]})(),
            self._core_api(wire),
        )
        assert service.get("pool-a:v2") is None


class TestCompositeDelegation:
    """The composite service must delegate the sidecar JWK reader to the
    backend that owns the workload record (review P1)."""

    def test_delegates_to_kubernetes_for_non_fsb(self) -> None:
        from unittest.mock import MagicMock

        from opensandbox_server.services.composite_service import CompositeSandboxService

        kubernetes = MagicMock()
        kubernetes.get_egress_signing_jwk.return_value = {"kty": "OKP"}
        fsb = MagicMock()
        fsb.get_egress_signing_jwk.return_value = None
        composite = CompositeSandboxService(kubernetes, fsb)

        assert composite.get_egress_signing_jwk("sbx-01K8CREATE") == {"kty": "OKP"}
        kubernetes.get_egress_signing_jwk.assert_called_once_with("sbx-01K8CREATE")
        assert composite.get_egress_signing_jwk("fsb-pool-a") is None
        fsb.get_egress_signing_jwk.assert_not_called()


class TestJWKSRoute:
    def make_app(self, service: JWKSService | None = None) -> TestClient:
        app = FastAPI()
        app.include_router(jwks_router)
        app.include_router(jwks_router, prefix="/v1")
        if service is not None:
            app.state.jwks_service = service
        return TestClient(app)

    def test_unconfigured_is_501(self) -> None:
        response = self.make_app().get("/credential-vault/jwks/pool-a:v2")
        assert response.status_code == 501

    def test_known_kid_200_unknown_404(self) -> None:
        kid, x = vectors_sidecar()
        service, _, _ = make_service(
            {"s": {"signing.pub": x.encode(), "kid": kid.encode()}}
        )
        client = self.make_app(service)
        ok = client.get(f"/credential-vault/jwks/{kid}")
        assert ok.status_code == 200
        assert ok.json() == {
            "kty": "OKP", "crv": "Ed25519", "x": x, "kid": kid,
            "alg": "EdDSA", "use": "sig",
        }
        assert "PRIVATE" not in ok.text
        assert client.get("/credential-vault/jwks/pool-z:v9").status_code == 404
        assert client.get("/credential-vault/jwks/..%2Fescape").status_code == 404

    def test_rate_limited_is_429(self) -> None:
        """Backend-budget exhaustion (unknown-kid flooding) answers 429;
        cached-key reads never consume that budget and never 429."""
        kid, x = vectors_sidecar()
        service, _, _ = make_service(
            {"s": {"signing.pub": x.encode(), "kid": kid.encode()}},
            max_lookups=2,
        )
        client = self.make_app(service)
        assert client.get(f"/credential-vault/jwks/{kid}").status_code == 200
        # Unknown kids burn the backend budget...
        statuses = [client.get(f"/credential-vault/jwks/sbx-flood{i:03d}").status_code for i in range(3)]
        assert 429 in statuses
        # ...while the cached key keeps serving.
        assert client.get(f"/credential-vault/jwks/{kid}").status_code == 200

    def test_endpoint_is_public(self) -> None:
        """No API key required (public-keys-only posture)."""
        assert "/credential-vault/jwks" in AuthMiddleware.EXEMPT_PATHS
        assert "/v1/credential-vault/jwks" in AuthMiddleware.EXEMPT_PATHS

    def test_served_under_both_root_and_v1(self) -> None:
        kid, x = vectors_sidecar()
        service, _, _ = make_service(
            {"s": {"signing.pub": x.encode(), "kid": kid.encode()}}
        )
        client = self.make_app(service)
        assert client.get(f"/credential-vault/jwks/{kid}").status_code == 200
        assert client.get(f"/v1/credential-vault/jwks/{kid}").status_code == 200
