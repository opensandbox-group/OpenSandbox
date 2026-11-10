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

"""Walk the shared resolver-auth vectors: the same file the Go mint-side
tests consume, so the two languages cannot drift on the wire contract."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from opensandbox_resolver_auth import VerifyError, VerifierConfig, verify_credential

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


def _vectors() -> dict:
    with VECTORS.open() as handle:
        return json.load(handle)


@pytest.fixture(scope="module")
def data() -> dict:
    return _vectors()


def test_vectors_present(data) -> None:
    assert data["schema"] == "opensandbox.resolver-auth.vectors/v1"
    assert len(data["cases"]) >= 26
    assert len(data["keys"]) == 2


def test_contract_constants_sync(data) -> None:
    from opensandbox_resolver_auth import (
        CREDENTIAL_HEADER,
        DEFAULT_ISSUER,
        KID_GRAMMAR,
        LEEWAY_SECONDS,
        TTL_MAX_SECONDS,
    )

    contract = data["contract"]
    assert contract["header_name"] == CREDENTIAL_HEADER
    assert contract["issuer"] == DEFAULT_ISSUER
    assert contract["kid_grammar"] == KID_GRAMMAR.pattern
    assert contract["leeway_seconds"] == LEEWAY_SECONDS
    assert contract["ttl_max_seconds"] == TTL_MAX_SECONDS


def _b64(segment: str) -> bytes:
    import base64

    return base64.urlsafe_b64decode(segment + "=" * (-len(segment) % 4))


def _case_parameters(data):
    for case in data["cases"]:
        for check in case["checks"]:
            yield pytest.param(case, check, id=f"{case['name']}@{check['now']}")


@pytest.mark.parametrize(("case", "check"), list(_case_parameters(_vectors())))
def test_vector_case(case, check, data) -> None:
    contract = data["contract"]
    # The JWK a verifying resolver would hold for this token's kid: the key
    # the case declares as its signer. (A sidecar keypair's JWK is published
    # under the sandbox's own kid, so the mixed-case-kid case resolves to the
    # same sidecar-1 public key.)
    signing = next(key for key in data["keys"] if key["id"] == case["key"])
    jwk = dict(signing["public_jwk"])
    if case["name"] == "reject_unknown_kid":
        jwk = None
    claims = json.loads(_b64(case["jwt"].split(".")[1]))

    config = VerifierConfig(
        jwks_base="https://server.invalid",
        expected_audience=case.get("expected_audience", contract["default_expected_audience"]),
    )
    document_subject = case.get("document_subject", claims["sub"])

    if check["verdict"] == "accept":
        subject = verify_credential(
            case["jwt"],
            jwk=jwk,
            document_subject=document_subject,
            config=config,
            now=check["now"],
        )
        assert subject == claims["sub"]
    else:
        with pytest.raises(VerifyError) as excinfo:
            verify_credential(
                case["jwt"],
                jwk=jwk,
                document_subject=document_subject,
                config=config,
                now=check["now"],
            )
        assert excinfo.value.code == check["error"], (
            f"{case['name']}: expected {check['error']}, got {excinfo.value.code}"
        )



def test_reject_non_string_and_malformed() -> None:
    config = VerifierConfig(jwks_base="https://server.invalid", expected_audience="a@b")
    for bad in (None, 123, "", "only-one-segment", "a.b", "a.b.c.d", "...", "a..c"):
        with pytest.raises(VerifyError) as excinfo:
            verify_credential(
                bad, jwk={}, document_subject="sbx-1", config=config, now=1
            )
        assert excinfo.value.code == "signature"
