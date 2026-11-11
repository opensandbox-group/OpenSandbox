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
"""OSEP-0009 renewalPolicy domain model and create-payload tests."""

from __future__ import annotations

from datetime import timedelta

import pytest
from pydantic import ValidationError

from opensandbox.adapters.converter.sandbox_model_converter import (
    SandboxModelConverter,
)
from opensandbox.models import RenewalPolicy, SandboxLifecycle


class TestRenewalPolicyValidation:
    def test_accepts_valid_range(self) -> None:
        assert RenewalPolicy(extend_seconds=300).extend_seconds == 300
        assert RenewalPolicy(extend_seconds=86400).extend_seconds == 86400

    def test_bounds_enforced(self) -> None:
        with pytest.raises(ValidationError):
            RenewalPolicy(extend_seconds=299)
        with pytest.raises(ValidationError):
            RenewalPolicy(extend_seconds=86401)

    def test_alias_population(self) -> None:
        policy = RenewalPolicy.model_validate({"extendSeconds": 3600})
        assert policy.extend_seconds == 3600

    def test_unknown_fields_rejected(self) -> None:
        with pytest.raises(ValidationError):
            RenewalPolicy.model_validate(
                {"extendSeconds": 3600, "onAccess": "renew"}
            )


def _create_request(lifecycle: SandboxLifecycle | None):
    return SandboxModelConverter.to_api_create_sandbox_request(
        spec=None,
        entrypoint=["python"],
        env={},
        metadata={},
        timeout=timedelta(hours=1),
        resource={},
        platform=None,
        network_policy=None,
        extensions=None,
        volumes=None,
        lifecycle=lifecycle,
    )


class TestCreatePayload:
    def test_renewal_policy_serialized_with_aliases(self) -> None:
        request = _create_request(
            SandboxLifecycle(renewal_policy=RenewalPolicy(extend_seconds=3600))
        )
        lifecycle_field = request.lifecycle
        assert lifecycle_field is not None
        assert lifecycle_field.to_dict() == {
            "renewalPolicy": {"extendSeconds": 3600}
        }

    def test_omitted_renewal_policy_keeps_lifecycle_unset(self) -> None:
        from opensandbox.api.lifecycle.types import UNSET, Unset

        request = _create_request(SandboxLifecycle())
        assert request.lifecycle is UNSET or isinstance(request.lifecycle, Unset)
