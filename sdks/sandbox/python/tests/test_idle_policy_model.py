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
"""OSEP-0024 idlePolicy domain model and create-payload tests."""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from opensandbox.adapters.converter.sandbox_model_converter import (
    SandboxModelConverter,
)
from opensandbox.models import ActivityProbe, IdlePolicy, SandboxLifecycle


class TestIdlePolicyValidation:
    def test_auto_pause_policy_accepts_valid_range(self) -> None:
        policy = IdlePolicy(idle_timeout_seconds=300, on_idle="pause")
        assert policy.idle_timeout_seconds == 300
        assert policy.wake_on_access is True
        assert policy.active_probe is None

    def test_timeout_bounds_enforced(self) -> None:
        with pytest.raises(ValidationError):
            IdlePolicy(idle_timeout_seconds=29, on_idle="pause")
        with pytest.raises(ValidationError):
            IdlePolicy(idle_timeout_seconds=86401, on_idle="pause")

    def test_on_idle_requires_timeout(self) -> None:
        with pytest.raises(ValidationError):
            IdlePolicy(on_idle="pause")

    def test_on_idle_only_accepts_pause(self) -> None:
        with pytest.raises(ValidationError):
            IdlePolicy(idle_timeout_seconds=300, on_idle="terminate")

    def test_wake_only_policy(self) -> None:
        policy = IdlePolicy()
        assert policy.idle_timeout_seconds is None
        assert policy.on_idle is None
        assert policy.wake_on_access is True

    def test_alias_population(self) -> None:
        policy = IdlePolicy.model_validate(
            {
                "idleTimeoutSeconds": 600,
                "onIdle": "pause",
                "wakeOnAccess": False,
                "activeProbe": {
                    "port": 8080,
                    "path": "/active",
                    "timeoutSeconds": 3,
                },
            }
        )
        assert policy.idle_timeout_seconds == 600
        assert policy.wake_on_access is False
        assert policy.active_probe is not None
        assert policy.active_probe.timeout_seconds == 3

    def test_unknown_fields_rejected(self) -> None:
        with pytest.raises(ValidationError):
            IdlePolicy.model_validate({"wakeOnAccess": True, "nope": 1})


class TestActivityProbeValidation:
    def test_port_bounds(self) -> None:
        with pytest.raises(ValidationError):
            ActivityProbe(port=0, path="/active")
        with pytest.raises(ValidationError):
            ActivityProbe(port=65536, path="/active")

    def test_path_must_be_absolute(self) -> None:
        with pytest.raises(ValidationError):
            ActivityProbe(port=8080, path="active")

    def test_path_must_not_contain_whitespace(self) -> None:
        with pytest.raises(ValidationError):
            ActivityProbe(port=8080, path="/ac tive")

    def test_timeout_bounds(self) -> None:
        with pytest.raises(ValidationError):
            ActivityProbe(port=8080, path="/a", timeout_seconds=0)
        with pytest.raises(ValidationError):
            ActivityProbe(port=8080, path="/a", timeout_seconds=11)


def _to_api_lifecycle(lifecycle: SandboxLifecycle | None):
    from opensandbox.api.lifecycle.types import UNSET, Unset

    request = SandboxModelConverter.to_api_create_sandbox_request(
        spec=None,
        entrypoint=None,
        env={},
        metadata={},
        timeout=None,
        resource={},
        platform=None,
        network_policy=None,
        extensions={},
        volumes=None,
        lifecycle=lifecycle,
    )
    lifecycle_field = request.lifecycle
    if lifecycle_field is None or isinstance(lifecycle_field, Unset):
        assert lifecycle_field is UNSET or lifecycle_field is None
        return {}
    return lifecycle_field.to_dict()


class TestCreatePayload:
    def test_idle_policy_serialized_with_aliases(self) -> None:
        payload = _to_api_lifecycle(
            SandboxLifecycle(
                idle_policy=IdlePolicy(
                    idle_timeout_seconds=300,
                    on_idle="pause",
                    wake_on_access=False,
                    active_probe=ActivityProbe(port=8080, path="/active"),
                )
            )
        )
        assert payload["idlePolicy"] == {
            "idleTimeoutSeconds": 300,
            "onIdle": "pause",
            "wakeOnAccess": False,
            "activeProbe": {"port": 8080, "path": "/active"},
        }
        assert "preStart" not in payload
        assert "periodic" not in payload

    def test_wake_only_policy_omits_on_idle(self) -> None:
        payload = _to_api_lifecycle(
            SandboxLifecycle(idle_policy=IdlePolicy())
        )
        assert payload["idlePolicy"] == {"wakeOnAccess": True}

    def test_omitted_idle_policy_sends_no_field(self) -> None:
        payload = _to_api_lifecycle(SandboxLifecycle())
        assert "idlePolicy" not in payload

    def test_hooks_and_policy_compose(self) -> None:
        from opensandbox.models import LifecycleHook

        payload = _to_api_lifecycle(
            SandboxLifecycle(
                pre_start=LifecycleHook(command=["sh", "-c", "true"]),
                idle_policy=IdlePolicy(idle_timeout_seconds=60, on_idle="pause"),
            )
        )
        assert payload["preStart"] == {"command": ["sh", "-c", "true"]}
        assert payload["idlePolicy"] == {
            "idleTimeoutSeconds": 60,
            "onIdle": "pause",
            "wakeOnAccess": True,
        }


class TestTemplateCreatePayload:
    def test_idle_policy_rides_template_create(self) -> None:
        from datetime import timedelta

        from opensandbox.api.lifecycle.types import Unset

        request = SandboxModelConverter.to_api_create_template_sandbox_request(
            template_id="tpl_1",
            timeout=timedelta(hours=1),
            metadata=None,
            network_policy=None,
            extensions=None,
            idle_policy=IdlePolicy(idle_timeout_seconds=30, on_idle="pause"),
        )
        lifecycle_field = request.lifecycle
        assert lifecycle_field is not None and not isinstance(
            lifecycle_field, Unset
        )
        assert lifecycle_field.to_dict() == {
            "idlePolicy": {
                "idleTimeoutSeconds": 30,
                "onIdle": "pause",
                "wakeOnAccess": True,
            }
        }

    def test_template_create_without_policy_keeps_lifecycle_unset(self) -> None:
        from datetime import timedelta

        from opensandbox.api.lifecycle.types import UNSET, Unset

        request = SandboxModelConverter.to_api_create_template_sandbox_request(
            template_id="tpl_1",
            timeout=timedelta(hours=1),
            metadata=None,
            network_policy=None,
            extensions=None,
        )
        assert request.lifecycle is UNSET or isinstance(request.lifecycle, Unset)
