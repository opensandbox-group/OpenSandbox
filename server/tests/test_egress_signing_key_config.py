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

"""OSEP-0025 D5: kubernetes.egress_signing_key_secrets config validation.

The list references the fast-sandbox pool signing Secrets (current version
first, previous retained for the dual-kid grace window); it must stay a
dedicated object set — no MITM CA sharing, no duplicates, no blank names.
"""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from opensandbox_server.config import KubernetesRuntimeConfig


def make(**overrides: object) -> KubernetesRuntimeConfig:
    return KubernetesRuntimeConfig(**overrides)


class TestEgressSigningKeySecretsConfig:
    def test_default_is_empty(self) -> None:
        config = make()
        assert config.egress_signing_key_secrets == []
        assert config.validate_egress_signing_key_secrets() is config

    def test_single_and_dual_version_lists_accepted(self) -> None:
        single = make(egress_signing_key_secrets=["opensandbox-egress-signing-pool-a-v2"])
        assert single.egress_signing_key_secrets == [
            "opensandbox-egress-signing-pool-a-v2"
        ]
        dual = make(
            egress_signing_key_secrets=[
                "opensandbox-egress-signing-pool-a-v3",
                "opensandbox-egress-signing-pool-a-v2",
            ]
        )
        assert len(dual.egress_signing_key_secrets) == 2

    def test_blank_name_rejected(self) -> None:
        with pytest.raises(ValidationError, match="non-empty"):
            make(egress_signing_key_secrets=["  "])

    def test_duplicate_rejected(self) -> None:
        with pytest.raises(ValidationError, match="unique"):
            make(
                egress_signing_key_secrets=[
                    "opensandbox-egress-signing-pool-a-v2",
                    "opensandbox-egress-signing-pool-a-v2",
                ]
            )

    def test_mitm_ca_overlap_rejected(self) -> None:
        with pytest.raises(ValidationError, match="dedicated Secrets"):
            make(
                template_mitm_ca_secret="opensandbox-egress-signing-pool-a-v2",
                egress_signing_key_secrets=["opensandbox-egress-signing-pool-a-v2"],
            )

    def test_mitm_ca_set_separately_is_fine(self) -> None:
        config = make(
            template_mitm_ca_secret="opensandbox-mitm-ca-v7",
            egress_signing_key_secrets=["opensandbox-egress-signing-pool-a-v2"],
        )
        assert config.template_mitm_ca_secret == "opensandbox-mitm-ca-v7"
