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

"""Request-schema validation for lifecycle.idlePolicy (OSEP-0024)."""

import pytest
from pydantic import ValidationError

from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    ImageSpec,
    IdlePolicy,
    LifecycleHook,
    ResourceLimits,
    SandboxLifecycle,
)


def _base_request(**overrides):
    payload = {
        "image": ImageSpec(uri="python:3.11"),
        "entrypoint": ["python"],
        "timeout": 3600,
        "resource_limits": ResourceLimits(root={"cpu": "500m"}),
    }
    payload.update(overrides)
    return CreateSandboxRequest(**payload)


def test_full_idle_policy_parses_with_aliases():
    lifecycle = SandboxLifecycle.model_validate(
        {
            "idlePolicy": {
                "idleTimeoutSeconds": 300,
                "onIdle": "pause",
                "wakeOnAccess": False,
                "activeProbe": {
                    "port": 8080,
                    "path": "/active",
                    "timeoutSeconds": 3,
                },
            }
        }
    )
    policy = lifecycle.idle_policy
    assert policy is not None
    assert policy.idle_timeout_seconds == 300
    assert policy.on_idle == "pause"
    assert policy.wake_on_access is False
    assert policy.active_probe is not None
    assert policy.active_probe.port == 8080
    assert policy.active_probe.path == "/active"
    assert policy.active_probe.timeout_seconds == 3


def test_wake_only_policy_has_no_timeout_and_defaults_wake():
    policy = IdlePolicy()
    assert policy.idle_timeout_seconds is None
    assert policy.on_idle is None
    assert policy.wake_on_access is True
    assert policy.active_probe is None


def test_probe_timeout_stays_optional_in_the_api_model():
    """The server applies the 2s default when persisting the policy."""
    policy = IdlePolicy(idle_timeout_seconds=30, on_idle="pause", active_probe={"port": 1, "path": "/x"})
    assert policy.active_probe is not None
    assert policy.active_probe.timeout_seconds is None


@pytest.mark.parametrize(
    "payload",
    [
        # onIdle without a timeout is not a valid wake-only spelling.
        {"onIdle": "pause"},
        # A probe without a timeout never runs: auto-pause is what probes.
        {"activeProbe": {"port": 8080, "path": "/active"}},
        # Unknown action; only "pause" exists.
        {"idleTimeoutSeconds": 300, "onIdle": "terminate"},
        {"idleTimeoutSeconds": 300},
        # Range bounds.
        {"idleTimeoutSeconds": 29, "onIdle": "pause"},
        {"idleTimeoutSeconds": 86401, "onIdle": "pause"},
        # Unknown fields are rejected, never ignored into an opt-in.
        {"idleTimeoutSeconds": 300, "onIdle": "pause", "unknown": True},
        # Probe shape.
        {
            "idleTimeoutSeconds": 300,
            "onIdle": "pause",
            "activeProbe": {"port": 8080, "path": "relative"},
        },
        {
            "idleTimeoutSeconds": 300,
            "onIdle": "pause",
            "activeProbe": {"port": 8080, "path": "/a b"},
        },
        {
            "idleTimeoutSeconds": 300,
            "onIdle": "pause",
            "activeProbe": {"port": 0, "path": "/ok"},
        },
        {
            "idleTimeoutSeconds": 300,
            "onIdle": "pause",
            "activeProbe": {"port": 8080, "path": "/ok", "timeoutSeconds": 11},
        },
    ],
)
def test_invalid_idle_policies_are_rejected(payload):
    with pytest.raises(ValidationError):
        IdlePolicy(**payload)


def test_template_mode_accepts_idle_policy_but_not_hooks():
    request = _base_request(
        template_id="tpl_1",
        image=None,
        entrypoint=None,
        resource_limits=None,
        lifecycle=SandboxLifecycle(idle_policy=IdlePolicy(idle_timeout_seconds=300, on_idle="pause")),
    )
    assert request.template_id == "tpl_1"

    with pytest.raises(ValidationError, match="templateId"):
        _base_request(
            template_id="tpl_1",
            image=None,
            entrypoint=None,
            resource_limits=None,
            lifecycle=SandboxLifecycle(pre_start=LifecycleHook(command=["true"])),
        )


def test_pool_mode_accepts_idle_policy_but_not_hooks():
    request = _base_request(
        extensions={"poolRef": "ml-pool"},
        lifecycle=SandboxLifecycle(idle_policy=IdlePolicy()),
    )
    assert request.lifecycle is not None

    with pytest.raises(ValidationError, match="poolRef"):
        _base_request(
            extensions={"poolRef": "ml-pool"},
            lifecycle=SandboxLifecycle(pre_start=LifecycleHook(command=["true"])),
        )
