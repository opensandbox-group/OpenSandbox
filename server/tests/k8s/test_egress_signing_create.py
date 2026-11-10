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

"""OSEP-0025 D5 W2.2 (K8s): per-sandbox keypair provisioning at create time.

_build_create_workload_context is the single choke point both K8s providers
share: when egress.resolver_caller_auth is enabled it generates the keypair,
injects the private PEM through the egress env (winning over any
request-supplied entry), and records the public JWK as a workload annotation.
"""

from __future__ import annotations

import base64
import json
from datetime import datetime, timezone

from cryptography.hazmat.primitives.serialization import (
    Encoding,
    PublicFormat,
    load_pem_private_key,
)

from opensandbox_server.api.schema import CreateSandboxRequest, ImageSpec, NetworkPolicy, ResourceLimits
from opensandbox_server.config import EgressConfig
from opensandbox_server.services.constants import (
    EGRESS_SIGNING_JWK_ANNOTATION,
    OPENSANDBOX_EGRESS_RESOLVER_SIGNING_KEY,
)
from opensandbox_server.services.k8s.create_helpers import (
    _build_create_workload_context,
)


def _b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def _pem_public_x(pem: str) -> str:
    key = load_pem_private_key(pem.encode(), password=None)
    return _b64url(key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw))


def _request(**overrides) -> CreateSandboxRequest:
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


def _context(k8s_app_config, request):
    return _build_create_workload_context(
        k8s_app_config,
        request,
        "sbx_01K8CREATE",
        datetime.now(timezone.utc),
        lambda: "egress-token",
        lambda: "secure-token",
    )


def test_disabled_by_default(k8s_app_config) -> None:
    k8s_app_config.egress = EgressConfig(image="egress:latest")
    context = _context(k8s_app_config, _request())
    assert context.egress_settings is not None
    assert OPENSANDBOX_EGRESS_RESOLVER_SIGNING_KEY not in (
        context.egress_settings.env or {}
    )
    assert EGRESS_SIGNING_JWK_ANNOTATION not in (context.annotations or {})


def test_enabled_injects_env_and_annotation(k8s_app_config) -> None:
    k8s_app_config.egress = EgressConfig(
        image="egress:latest", resolver_caller_auth=True
    )
    context = _context(k8s_app_config, _request())
    pem = context.egress_settings.env[OPENSANDBOX_EGRESS_RESOLVER_SIGNING_KEY]
    assert pem.startswith("-----BEGIN PRIVATE KEY-----")
    jwk = json.loads(context.annotations[EGRESS_SIGNING_JWK_ANNOTATION])
    assert jwk["kid"] == "sbx_01K8CREATE"
    assert jwk["x"] == _pem_public_x(pem), (
        "the published JWK must be the public half of the injected PEM"
    )


def test_request_supplied_signing_env_rejected(k8s_app_config) -> None:
    k8s_app_config.egress = EgressConfig(
        image="egress:latest", resolver_caller_auth=True
    )
    request = _request(env={OPENSANDBOX_EGRESS_RESOLVER_SIGNING_KEY: "attacker pem"})
    try:
        _context(k8s_app_config, request)
    except ValueError as exc:
        assert OPENSANDBOX_EGRESS_RESOLVER_SIGNING_KEY in str(exc)
        assert "not allowed" in str(exc)
    else:
        raise AssertionError(
            "request-supplied caller-auth signing env must be rejected by "
            "the egress env allowlist"
        )


def test_no_provisioning_without_network_policy(k8s_app_config) -> None:
    k8s_app_config.egress = EgressConfig(
        image="egress:latest", resolver_caller_auth=True
    )
    context = _context(k8s_app_config, _request(networkPolicy=None))
    assert context.egress_settings is None
    assert EGRESS_SIGNING_JWK_ANNOTATION not in (context.annotations or {})
