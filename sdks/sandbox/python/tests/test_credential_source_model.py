#
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
#
"""OSEP-0025 credential source / typeless auth model validation tests."""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from opensandbox.models import Credential, CredentialAuth, ExchangeCredentialSource


class TestCredentialSourceVariants:
    def test_exchange_source_accepts_endpoint_only(self) -> None:
        source = ExchangeCredentialSource(endpoint="unix:///run/spi/spi.sock")
        assert source.type == "exchange"
        assert "endpoint" in repr(source)

    def test_exchange_source_rejects_inline_material(self) -> None:
        # Cross-variant keys must fail fast client-side instead of being
        # silently dropped from the outgoing payload.
        with pytest.raises(ValidationError):
            Credential.model_validate(
                {
                    "name": "spi-signer",
                    "source": {
                        "type": "exchange",
                        "endpoint": "unix:///run/spi/spi.sock",
                        "value": "secret",
                    },
                }
            )

    def test_inline_source_rejects_endpoint(self) -> None:
        with pytest.raises(ValidationError):
            Credential.model_validate(
                {
                    "name": "tok",
                    "source": {"type": "inline", "value": "secret", "endpoint": "unix:///x"},
                }
            )

    def test_valid_variants_round_trip(self) -> None:
        exchange = Credential.model_validate(
            {
                "name": "spi-signer",
                "source": {"type": "exchange", "endpoint": "https://resolver.internal"},
            }
        )
        dumped = exchange.model_dump(by_alias=True, exclude_none=True)
        assert dumped["source"] == {
            "type": "exchange",
            "endpoint": "https://resolver.internal",
        }


class TestTypelessCredentialAuth:
    def test_typeless_form_accepts_credential_only(self) -> None:
        auth = CredentialAuth.model_validate({"credential": "spi-signer"})
        dumped = auth.model_dump(by_alias=True, exclude_none=True)
        assert dumped == {"credential": "spi-signer"}

    def test_empty_auth_is_rejected(self) -> None:
        with pytest.raises(ValidationError):
            CredentialAuth.model_validate({})

    def test_typeless_form_rejects_static_fields(self) -> None:
        with pytest.raises(ValidationError):
            CredentialAuth.model_validate(
                {"credential": "spi-signer", "substitutions": []}
            )
        with pytest.raises(ValidationError):
            CredentialAuth.model_validate({"credential": "spi-signer", "name": "X-Api-Key"})
        with pytest.raises(ValidationError):
            CredentialAuth.model_validate(
                {
                    "credential": "spi-signer",
                    "headers": [{"name": "X-Sign", "credential": "spi-signer"}],
                }
            )

    def test_static_kinds_still_validate(self) -> None:
        passthrough = CredentialAuth.model_validate({"type": "passthrough"})
        assert passthrough.model_dump(by_alias=True, exclude_none=True) == {
            "type": "passthrough"
        }
        bearer = CredentialAuth.model_validate({"type": "bearer", "credential": "tok"})
        assert bearer.model_dump(by_alias=True, exclude_none=True) == {
            "type": "bearer",
            "credential": "tok",
        }
