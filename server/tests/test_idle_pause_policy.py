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

import json

import pytest

from opensandbox_server.integrations.idle_pause.policy import (
    ActivityProbe,
    IdlePolicy,
    idle_policy_from_annotation,
    idle_policy_to_annotation,
    validate_idle_policy,
)


def test_wake_only_policy_when_timeout_absent():
    policy = validate_idle_policy(idle_timeout_seconds=None, on_idle=None)
    assert policy.idle_timeout_seconds is None
    assert policy.on_idle is None
    assert policy.auto_pause_enabled is False
    assert policy.wake_on_access is True


def test_on_idle_requires_timeout():
    with pytest.raises(ValueError, match="onIdle must be omitted"):
        validate_idle_policy(idle_timeout_seconds=None, on_idle="pause")


def test_timeout_bounds():
    validate_idle_policy(idle_timeout_seconds=30, on_idle="pause")
    validate_idle_policy(idle_timeout_seconds=86400, on_idle="pause")
    with pytest.raises(ValueError, match="between 30 and 86400"):
        validate_idle_policy(idle_timeout_seconds=29, on_idle="pause")
    with pytest.raises(ValueError, match="between 30 and 86400"):
        validate_idle_policy(idle_timeout_seconds=86401, on_idle="pause")
    with pytest.raises(ValueError, match="must be an integer"):
        validate_idle_policy(idle_timeout_seconds=True, on_idle="pause")


def test_timeout_must_not_exceed_activity_ttl():
    with pytest.raises(ValueError, match="activity_ttl_seconds"):
        validate_idle_policy(
            idle_timeout_seconds=300,
            on_idle="pause",
            activity_ttl_seconds=299,
        )
    assert validate_idle_policy(
        idle_timeout_seconds=300, on_idle="pause", activity_ttl_seconds=300
    ).auto_pause_enabled


def test_on_idle_only_pause_accepted():
    assert validate_idle_policy(idle_timeout_seconds=60, on_idle="pause").auto_pause_enabled
    with pytest.raises(ValueError, match="must be 'pause'"):
        validate_idle_policy(idle_timeout_seconds=60, on_idle="terminate")
    with pytest.raises(ValueError, match="must be 'pause'"):
        validate_idle_policy(idle_timeout_seconds=60, on_idle=None)


def test_wake_on_access_must_be_boolean():
    """A non-bool would survive the write and void the whole declaration on read."""
    for bad in ("true", 1, None):
        with pytest.raises(ValueError, match="wakeOnAccess must be a boolean"):
            validate_idle_policy(
                idle_timeout_seconds=60, on_idle="pause", wake_on_access=bad  # type: ignore[arg-type]
            )
    assert validate_idle_policy(idle_timeout_seconds=60, on_idle="pause", wake_on_access=False)


def test_probe_validation():
    probe = ActivityProbe(port=8080, path="/active", timeout_seconds=2)
    policy = validate_idle_policy(idle_timeout_seconds=300, on_idle="pause", active_probe=probe)
    assert policy.active_probe == probe

    with pytest.raises(ValueError, match="port"):
        validate_idle_policy(
            idle_timeout_seconds=300,
            on_idle="pause",
            active_probe=ActivityProbe(port=0, path="/active"),
        )
    with pytest.raises(ValueError, match="path"):
        validate_idle_policy(
            idle_timeout_seconds=300,
            on_idle="pause",
            active_probe=ActivityProbe(port=8080, path="active"),
        )
    with pytest.raises(ValueError, match="path"):
        validate_idle_policy(
            idle_timeout_seconds=300,
            on_idle="pause",
            active_probe=ActivityProbe(port=8080, path="/a b"),
        )
    with pytest.raises(ValueError, match="timeoutSeconds"):
        validate_idle_policy(
            idle_timeout_seconds=300,
            on_idle="pause",
            active_probe=ActivityProbe(port=8080, path="/active", timeout_seconds=11),
        )


def test_annotation_roundtrip():
    policy = validate_idle_policy(
        idle_timeout_seconds=300,
        on_idle="pause",
        wake_on_access=False,
        active_probe=ActivityProbe(port=8080, path="/active?deep=1", timeout_seconds=3),
    )
    decoded = idle_policy_from_annotation(idle_policy_to_annotation(policy))
    assert decoded == policy


def test_annotation_roundtrip_wake_only():
    policy = validate_idle_policy(idle_timeout_seconds=None)
    decoded = idle_policy_from_annotation(idle_policy_to_annotation(policy))
    assert decoded == policy
    assert decoded is not None and decoded.auto_pause_enabled is False


def test_annotation_shape_matches_ingress_parser():
    """The JSON the ingress wake gate parses must accept our encoding."""
    value = idle_policy_to_annotation(
        validate_idle_policy(idle_timeout_seconds=300, on_idle="pause")
    )
    assert json.loads(value) == {
        "idleTimeoutSeconds": 300,
        "onIdle": "pause",
        "wakeOnAccess": True,
    }


def test_annotation_absent_or_blank_returns_none():
    assert idle_policy_from_annotation(None) is None
    assert idle_policy_from_annotation("") is None
    assert idle_policy_from_annotation("   ") is None
    assert idle_policy_from_annotation("null") is None  # not an implicit opt-in


def test_annotation_parses_ingress_shaped_values():
    """Values the ingress accepts must decode identically here."""
    assert idle_policy_from_annotation('{"wakeOnAccess":true}') == IdlePolicy()
    assert idle_policy_from_annotation("{}") == IdlePolicy()
    assert idle_policy_from_annotation('{"wakeOnAccess":false}') == IdlePolicy(wake_on_access=False)
    assert idle_policy_from_annotation(
        '{"idleTimeoutSeconds":600,"onIdle":"pause"}'
    ) == IdlePolicy(idle_timeout_seconds=600, on_idle="pause")
    assert idle_policy_from_annotation(
        '{"idleTimeoutSeconds":600,"onIdle":"pause","activeProbe":'
        '{"port":9090,"path":"/busy","timeoutSeconds":5}}'
    ) == IdlePolicy(
        idle_timeout_seconds=600,
        on_idle="pause",
        active_probe=ActivityProbe(port=9090, path="/busy", timeout_seconds=5),
    )


@pytest.mark.parametrize(
    "raw",
    [
        "not json",
        '{"idleTimeoutSeconds":300,"onIdle":"pause"} trailing',
        "[1,2]",  # not an object
        '{"unknownField":1}',  # unknown field (ingress DisallowUnknownFields)
        '{"onIdle":"pause"}',  # onIdle without timeout
        '{"idleTimeoutSeconds":300}',  # timeout without onIdle
        '{"idleTimeoutSeconds":300,"onIdle":"terminate"}',  # unsupported action
        '{"idleTimeoutSeconds":29,"onIdle":"pause"}',  # out of range: absent
        '{"idleTimeoutSeconds":86401,"onIdle":"pause"}',
        '{"idleTimeoutSeconds":"300","onIdle":"pause"}',  # wrong type
        '{"wakeOnAccess":"yes"}',  # wrong type
        '{"wakeOnAccess":1}',  # int masquerading as bool
        '{"activeProbe":{"port":0,"path":"/x"}}',  # invalid probe port
        '{"activeProbe":{"port":8080,"path":"x"}}',  # relative probe path
        '{"activeProbe":{"port":8080,"path":"/x","timeoutSeconds":99}}',
        '{"activeProbe":{"nope":1}}',
        # Same path bounds as the validator: kubectl annotate bypasses create.
        '{"activeProbe":{"port":8080,"path":"/x\\r\\nHost: evil"}}',
        f'{{"activeProbe":{{"port":8080,"path":"/{ "a" * 600 }"}}}}',
    ],
)
def test_annotation_invalid_fails_safe(raw):
    """Undecodable/out-of-range annotation behaves as no policy: never pause, never wake."""
    assert idle_policy_from_annotation(raw) is None
