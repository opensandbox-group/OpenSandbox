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

"""Docker-sidecar caller-authentication end-to-end, no cluster required.

Real server components throughout; mocks only at the Docker API and the
SDK's HTTP transport: create_sandbox provisions the per-sandbox keypair,
the JWKS route serves the recorded container label, and the resolver SDK
verifies a credential minted with the injected private key. Covers the
rotation story too — recreating a sandbox replaces the keypair and the
predecessor's credential stops verifying.
"""

from __future__ import annotations

import base64
import json
import sys
import time
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import load_pem_private_key
from fastapi import FastAPI
from fastapi.testclient import TestClient

from opensandbox_server.api.jwks import router as jwks_router
from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    CredentialProxyConfig,
    ImageSpec,
    NetworkPolicy,
    ResourceLimits,
)
from opensandbox_server.config import (
    AppConfig,
    EgressConfig,
    IngressConfig,
    RuntimeConfig,
    ServerConfig,
)
from opensandbox_server.services.constants import (
    EGRESS_SIGNING_JWK_ANNOTATION,
    OPENSANDBOX_EGRESS_RESOLVER_SIGNING_KEY,
)

# The resolver SDK is not a server dependency; import it from the repo tree.
_SDK_SRC = Path(__file__).resolve().parents[1].parent / "sdks" / "sandbox" / "python" / "src"
if _SDK_SRC.is_dir():
    sys.path.insert(0, str(_SDK_SRC))

from opensandbox.resolver_auth import CredentialVaultCallerAuth, VerifyError  # noqa: E402

RESOLVER_ORIGIN = "https://resolver.internal:8443"


class _nullcontext:
    """contextlib.nullcontext equivalent (keep the test file import-light)."""

    def __enter__(self):
        return None

    def __exit__(self, *_exc):
        return False


def _b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def _mint(pem: str, *, kid: str, subject: str, audience: str = RESOLVER_ORIGIN) -> str:
    """Mint the credential the egress would attach for (kid, subject)."""
    now = int(time.time())
    header = _b64url(json.dumps({"alg": "EdDSA", "typ": "JWT", "kid": kid}).encode())
    claims = _b64url(
        json.dumps(
            {
                "iss": "opensandbox",
                "aud": audience,
                "sub": subject,
                "iat": now,
                "exp": now + 3600,
                "jti": _b64url(b"e2e-jti-0123456789ab"),
            },
            separators=(",", ":"),
        ).encode()
    )
    key = load_pem_private_key(pem.encode(), password=None)
    assert isinstance(key, Ed25519PrivateKey)
    signature = key.sign(f"{header}.{claims}".encode())
    return f"{header}.{claims}.{_b64url(signature)}"


class _SidecarE2E:
    """One mocked-Docker server whose sidecar container is inspectable."""

    def __init__(self) -> None:
        from opensandbox_server.services.docker.docker_service import (
            DockerSandboxService,
        )

        self.mock_client = MagicMock()
        self.mock_client.containers.list.return_value = []
        self.mock_client.api.create_host_config.side_effect = lambda **kwargs: kwargs
        self.mock_client.api.create_container.side_effect = [
            {"Id": "sidecar-id"},
            {"Id": "main-id"},
        ]
        sidecar_stub = MagicMock(id="sidecar-id")
        self._sidecar_stub = sidecar_stub
        self.mock_client.containers.get.side_effect = lambda name: sidecar_stub

        cfg = AppConfig(
            server=ServerConfig(),
            runtime=RuntimeConfig(
                type="docker", execd_image="ghcr.io/opensandbox/platform:latest"
            ),
            ingress=IngressConfig(mode="direct"),
        )
        cfg.docker.network_mode = "bridge"
        cfg.egress = EgressConfig(image="egress:latest", mode="dns+nft")
        with patch(
            "opensandbox_server.services.docker.docker_service.docker"
        ) as mock_docker:
            mock_docker.from_env.return_value = self.mock_client
            self.service = DockerSandboxService(config=cfg)

    async def create(self, *, sandbox_id: str | None = None) -> tuple[str, str, dict]:
        """Create one sandbox; return (sandbox_id, private_pem, jwk).

        ``sandbox_id`` models recreation: forcing the same external id
        across two creates.
        """
        self.mock_client.api.create_container.side_effect = [
            {"Id": "sidecar-id"},
            {"Id": "main-id"},
        ]
        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=120,
            resourceLimits=ResourceLimits(root={}),
            env={},
            metadata={},
            entrypoint=["python"],
            networkPolicy=NetworkPolicy(default_action="deny", egress=[]),
            credentialProxy=CredentialProxyConfig(enabled=True),
        )
        id_patch = (
            patch.object(self.service, "generate_sandbox_id", return_value=sandbox_id)
            if sandbox_id is not None
            else _nullcontext()
        )
        with (
            id_patch,
            patch(
                "opensandbox_server.services.docker.docker_service.generate_egress_token",
                return_value="egress-token",
            ),
            patch(
                "opensandbox_server.services.docker.docker_service.allocate_port_bindings",
                return_value={
                    "44772": ("0.0.0.0", 44772),
                    "8080": ("0.0.0.0", 8080),
                    "18080": ("0.0.0.0", 18080),
                },
            ),
            patch.object(self.service, "_ensure_image_available"),
            patch.object(self.service, "_prepare_sandbox_runtime"),
            patch.object(self.service, "_wait_for_egress_sidecar_ready"),
        ):
            await self.service.create_sandbox(request)

        # Each create round appends (sidecar, main); the sidecar of THIS
        # round is second-to-last.
        sidecar_kwargs = self.mock_client.api.create_container.call_args_list[-2].kwargs
        env = dict(
            entry.split("=", 1) for entry in sidecar_kwargs["environment"]
        )
        pem = env[OPENSANDBOX_EGRESS_RESOLVER_SIGNING_KEY]
        sandbox_id = sidecar_kwargs["labels"]["opensandbox.io/egress-sidecar-for"]
        jwk = json.loads(sidecar_kwargs["labels"][EGRESS_SIGNING_JWK_ANNOTATION])
        # The inspectable sidecar carries exactly the created label set.
        self._sidecar_stub.labels = dict(sidecar_kwargs["labels"])
        return sandbox_id, pem, jwk

    def jwks_client(self) -> TestClient:
        from opensandbox_server.services.jwks import build_sidecar_only_jwks_service

        app = FastAPI()
        app.include_router(jwks_router)
        app.state.jwks_service = build_sidecar_only_jwks_service(
            self.service.get_egress_signing_jwk
        )
        return TestClient(app)


def _caller_auth(http: TestClient) -> CredentialVaultCallerAuth:
    """SDK instance whose JWKS transport rides the route's TestClient."""

    def transport(url: str, *, timeout: float):
        path = url.split("/credential-vault", 1)[1]
        response = http.get("/credential-vault" + path)
        return response.status_code, response.content

    return CredentialVaultCallerAuth(
        jwks_base="https://server.invalid/credential-vault",
        expected_audience=RESOLVER_ORIGIN,
        transport=transport,
    )


class TestDockerSidecarCallerAuthE2E:
    @pytest.mark.asyncio
    async def test_full_chain_provision_to_verification(self) -> None:
        env = _SidecarE2E()
        sandbox_id, pem, jwk = await env.create()

        # 1. Provisioning: kid = sandboxId, published JWK pairs with the PEM.
        assert jwk["kid"] == sandbox_id

        # 2. Publication: the JWKS route serves the recorded label via the
        #    diagnostics-mixin sidecar reader.
        http = env.jwks_client()
        response = http.get(f"/credential-vault/jwks/{sandbox_id}")
        assert response.status_code == 200
        assert response.json() == jwk

        # 3. Verification: the SDK verifies a credential minted with the
        #    injected private key and returns the attested subject.
        subject = _caller_auth(http).verify_caller(
            _mint(pem, kid=sandbox_id, subject=sandbox_id),
            document_subject=sandbox_id,
        )
        assert subject == sandbox_id

    @pytest.mark.asyncio
    async def test_wrong_subject_rejected(self) -> None:
        env = _SidecarE2E()
        sandbox_id, pem, _ = await env.create()
        http = env.jwks_client()
        with pytest.raises(VerifyError) as excinfo:
            _caller_auth(http).verify_caller(
                _mint(pem, kid=sandbox_id, subject="sbx-imposter"),
                document_subject=sandbox_id,
            )
        assert excinfo.value.code == "subject_mismatch"

    @pytest.mark.asyncio
    async def test_recreation_replaces_keypair_and_revokes_old_credential(self) -> None:
        env = _SidecarE2E()
        sandbox_id = "sbx-recreat"
        first_id, first_pem, first_jwk = await env.create(sandbox_id=sandbox_id)
        old_credential = _mint(first_pem, kid=first_id, subject=first_id)
        assert first_id == sandbox_id

        # Same sandboxId recreated: fresh keypair, same kid.
        second_id, second_pem, second_jwk = await env.create(sandbox_id=sandbox_id)
        assert second_id == sandbox_id
        assert second_pem != first_pem
        assert second_jwk["x"] != first_jwk["x"]

        # The JWKS entry now serves the new key only — the credential minted
        # by the predecessor fails signature verification.
        http = env.jwks_client()
        assert http.get(f"/credential-vault/jwks/{sandbox_id}").json() == second_jwk
        with pytest.raises(VerifyError) as excinfo:
            _caller_auth(http).verify_caller(old_credential, document_subject=sandbox_id)
        assert excinfo.value.code == "signature"

        # The successor's credential verifies.
        subject = _caller_auth(http).verify_caller(
            _mint(second_pem, kid=sandbox_id, subject=sandbox_id),
            document_subject=sandbox_id,
        )
        assert subject == sandbox_id

    @pytest.mark.asyncio
    async def test_unknown_sandbox_is_404(self) -> None:
        env = _SidecarE2E()
        await env.create()
        http = env.jwks_client()
        assert http.get("/credential-vault/jwks/sbx-never-created").status_code == 404
