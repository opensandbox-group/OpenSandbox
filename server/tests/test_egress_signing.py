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

"""OSEP-0025 D5 batch 2: per-sandbox caller-auth provisioning (W2.2).

Material unit tests (generation, pairing, grammar, recreation) and the
Docker sidecar path (env + container label). The K8s create-path tests live
in tests/k8s/test_egress_signing_create.py where the k8s fixtures live.
"""

from __future__ import annotations

import base64
import json
from unittest.mock import MagicMock, patch

import pytest
from cryptography.hazmat.primitives.serialization import (
    Encoding,
    PublicFormat,
    load_pem_private_key,
)

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
from opensandbox_server.services.egress_signing import (
    generate_sidecar_signing_material,
    parse_recorded_jwk,
)


def pem_public_raw(pem: str) -> bytes:
    key = load_pem_private_key(pem.encode(), password=None)
    return key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)


def b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


class TestSidecarSigningMaterial:
    def test_kid_is_sandbox_id_and_jwk_shape(self) -> None:
        material = generate_sidecar_signing_material("sbx_01HZYEXAMPLE")
        assert material.kid == "sbx_01HZYEXAMPLE"
        assert material.jwk == {
            "kty": "OKP",
            "crv": "Ed25519",
            "x": b64url(pem_public_raw(material.private_pem)),
            "kid": "sbx_01HZYEXAMPLE",
            "alg": "EdDSA",
            "use": "sig",
        }
        assert material.private_pem.startswith("-----BEGIN PRIVATE KEY-----")

    def test_private_pem_pairs_with_published_x(self) -> None:
        material = generate_sidecar_signing_material("sbx-pairs1")
        # The published x must be the public half of the injected private key.
        assert material.jwk["x"] == b64url(pem_public_raw(material.private_pem))

    def test_recreation_replaces_keypair(self) -> None:
        first = generate_sidecar_signing_material("sbx-recreat")
        second = generate_sidecar_signing_material("sbx-recreat")
        assert first.private_pem != second.private_pem
        assert first.jwk["x"] != second.jwk["x"], (
            "sandbox recreation must replace the keypair — the new JWKS entry "
            "displaces the old one"
        )

    def test_off_grammar_sandbox_id_rejected(self) -> None:
        for bad in ("../escape", "sbx bad", "sbx/1", "a:b:c"):
            with pytest.raises(ValueError, match="kid grammar"):
                generate_sidecar_signing_material(bad)

    def test_parse_recorded_jwk_matrix(self) -> None:
        material = generate_sidecar_signing_material("sbx-record1")
        assert parse_recorded_jwk(material.jwk_json) == material.jwk
        assert parse_recorded_jwk("not json") is None
        assert parse_recorded_jwk('{"kty":"RSA","kid":"k","x":"x"}') is None
        assert parse_recorded_jwk('{"kty":"OKP"}') is None


def make_request(**overrides):
    payload = dict(
        image=ImageSpec(uri="python:3.11"),
        timeout=120,
        resourceLimits=ResourceLimits(root={}),
        env={},
        metadata={},
        entrypoint=["python"],
        networkPolicy=NetworkPolicy(default_action="deny", egress=[]),
    )
    payload.update(overrides)
    return CreateSandboxRequest(**payload)


class TestDockerSidecarProvisioning:
    @pytest.mark.asyncio
    @patch("opensandbox_server.services.docker.docker_service.docker")
    async def test_enabled_adds_env_and_label(self, mock_docker):
        from opensandbox_server.services.docker.docker_service import (
            DockerSandboxService,
        )

        mock_client = MagicMock()
        mock_client.containers.list.return_value = []
        mock_client.api.create_host_config.side_effect = lambda **kwargs: kwargs
        mock_client.api.create_container.side_effect = [
            {"Id": "sidecar-id"},
            {"Id": "main-id"},
        ]
        mock_client.containers.get.side_effect = [
            MagicMock(id="sidecar-id"),
            MagicMock(id="main-id"),
        ]
        mock_docker.from_env.return_value = mock_client

        cfg = AppConfig(
            server=ServerConfig(),
            runtime=RuntimeConfig(
                type="docker", execd_image="ghcr.io/opensandbox/platform:latest"
            ),
            ingress=IngressConfig(mode="direct"),
        )
        cfg.docker.network_mode = "bridge"
        cfg.egress = EgressConfig(image="egress:latest", mode="dns+nft")
        service = DockerSandboxService(config=cfg)

        req = make_request(credentialProxy=CredentialProxyConfig(enabled=True))
        with (
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
            patch.object(service, "_ensure_image_available"),
            patch.object(service, "_prepare_sandbox_runtime"),
            patch.object(service, "_wait_for_egress_sidecar_ready"),
        ):
            await service.create_sandbox(req)

        sidecar_call = mock_client.api.create_container.call_args_list[0]
        kwargs = sidecar_call.kwargs
        env_entries = {
            entry.split("=", 1)[0]: entry.split("=", 1)[1]
            for entry in kwargs["environment"]
        }
        assert env_entries[OPENSANDBOX_EGRESS_RESOLVER_SIGNING_KEY].startswith(
            "-----BEGIN PRIVATE KEY-----"
        )
        recorded = kwargs["labels"][EGRESS_SIGNING_JWK_ANNOTATION]
        jwk = json.loads(recorded)
        assert jwk["kid"] == kwargs["labels"]["opensandbox.io/egress-sidecar-for"]
        assert jwk["x"] == b64url(
            pem_public_raw(env_entries[OPENSANDBOX_EGRESS_RESOLVER_SIGNING_KEY])
        )
