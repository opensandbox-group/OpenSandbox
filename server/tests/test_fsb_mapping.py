# pyright: reportAttributeAccessIssue=false
# protobuf-generated modules expose dynamic attributes.

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

"""Unit tests for fsb create/status mapping."""

from datetime import datetime, timezone
import json

import pytest

from opensandbox_server.api.schema import (
    CredentialProxyConfig,
    ImageSpec,
    LifecycleHook,
    NetworkPolicy,
    NetworkRule,
    PlatformSpec,
    RenewalPolicy,
    ResourceLimits,
    SandboxLifecycle,
    Volume,
)
from opensandbox_server.services.fast_sandbox.create_mapping import (
    UnsupportedFieldError,
    map_create_request,
    map_template_create_request,
)
from opensandbox_server.services.fast_sandbox.generated import fastpath_pb2 as pb2
from opensandbox_server.services.fast_sandbox.status_mapping import map_reason, map_state

NOW = datetime(2026, 8, 18, 12, 0, 0, tzinfo=timezone.utc)
EXPECTED_EXPIRY = int(NOW.timestamp()) + 3600


def _host_volume():
    from opensandbox_server.api.schema import Host

    return Volume(name="data", host=Host(path="/tmp/data"), mountPath="/data")


def _auth():
    from opensandbox_server.api.schema import ImageAuth

    return ImageAuth(username="u", password="p")


def _base_request(**overrides):
    from opensandbox_server.api.schema import CreateSandboxRequest

    payload = {
        "image": ImageSpec(uri="python:3.11"),
        "entrypoint": ["python", "-m", "http.server"],
        "timeout": 3600,
        "resource_limits": ResourceLimits(root={"cpu": "500m"}),
    }
    payload.update(overrides)
    return CreateSandboxRequest(**payload)


def test_map_create_request_maps_core_fields():
    request = _base_request(
        env={"PYTHONUNBUFFERED": "1"},
        metadata={"team": "agents"},
        extensions={"poolRef": "ml-pool"},
    )

    create = map_create_request(request, "sbx-1", "ns-1", now=NOW)

    assert create.request_id == "sbx-1"
    assert create.namespace == "ns-1"
    assert create.image == "python:3.11"
    assert create.command == ["python", "-m", "http.server"]
    assert create.envs["PYTHONUNBUFFERED"] == "1"
    assert create.metadata["team"] == "agents"
    assert create.pool_ref == "ml-pool"
    assert create.expires_at_unix_seconds == EXPECTED_EXPIRY
    assert create.completion == pb2.CREATE_COMPLETION_READY


def test_map_create_request_defaults_pool_ref():
    request = _base_request()
    create = map_create_request(request, "sbx-1", "ns-1", now=NOW)
    assert create.pool_ref == "default-pool"


def test_create_includes_network_policy_in_atomic_intent():
    policy = NetworkPolicy(egress=[NetworkRule(action=" Allow ", target=" *.example.com ")])
    create = map_create_request(_base_request(network_policy=policy), "sbx-1", "ns-1", now=NOW)
    assert len(create.action_bindings) == 1
    assert create.action_bindings[0].handler == "egress"
    assert json.loads(create.action_bindings[0].input) == {
        "defaultAction": "deny",
        "egress": [{"action": "allow", "target": "*.example.com"}],
    }


def test_map_create_request_strips_pool_ref():
    blank = map_create_request(
        _base_request(extensions={"poolRef": "   "}), "sbx-1", "ns-1", now=NOW
    )
    assert blank.pool_ref == "default-pool"

    padded = map_create_request(
        _base_request(extensions={"poolRef": " ml-pool "}), "sbx-1", "ns-1", now=NOW
    )
    assert padded.pool_ref == "ml-pool"


@pytest.mark.parametrize(
    "field_name,payload",
    [
        ("platform", {"platform": PlatformSpec(os="linux", arch="amd64")}),
        (
            "resourceRequests",
            {"resource_requests": ResourceLimits(root={"cpu": "1"})},
        ),
        ("secureAccess", {"secure_access": True}),
        ("volumes", {"volumes": [_host_volume()]}),
        (
            "lifecycle",
            {
                "lifecycle": SandboxLifecycle(
                    preStart=LifecycleHook(command=["true"]),
                )
            },
        ),
        (
            "lifecycle.renewalPolicy",
            {
                "lifecycle": SandboxLifecycle(
                    renewalPolicy=RenewalPolicy(extendSeconds=300),
                )
            },
        ),
    ],
)
def test_map_create_request_rejects_unsupported_fields(field_name, payload):
    request = _base_request(**payload)
    with pytest.raises(UnsupportedFieldError) as exc_info:
        map_create_request(request, "sbx-1", "ns-1", now=NOW)
    assert exc_info.value.field == field_name


def test_map_create_request_accepts_credential_proxy_ignored():
    """OSEP-0022: credentialProxy no longer rejects the fsb create; the vault
    is not carried at create time and rides the relay after creation."""
    request = _base_request(
        credential_proxy=CredentialProxyConfig(enabled=True),
        network_policy=NetworkPolicy(egress=[NetworkRule(action="allow", target="a.com")]),
    )
    create = map_create_request(request, "sbx-1", "ns-1", now=NOW)
    assert create.request_id == "sbx-1"
    assert len(create.action_bindings) == 1
    assert create.action_bindings[0].handler == "egress"


def test_map_create_request_accepts_idle_policy_only_lifecycle():
    """idlePolicy is control-plane state; the mapping ignores it, the service persists it."""
    from opensandbox_server.api.schema import IdlePolicy

    request = _base_request(
        lifecycle=SandboxLifecycle(
            idlePolicy=IdlePolicy(idle_timeout_seconds=300, on_idle="pause"),
        )
    )
    create = map_create_request(request, "sbx-1", "ns-1", now=NOW)
    assert create.request_id == "sbx-1"


def test_map_create_request_rejects_image_auth():
    request = _base_request(image=ImageSpec(uri="private/reg:1", auth=_auth()))
    with pytest.raises(UnsupportedFieldError) as exc_info:
        map_create_request(request, "sbx-1", "ns-1", now=NOW)
    assert exc_info.value.field == "image.auth"


def test_map_create_request_rejects_missing_image_even_with_pool_ref():
    request = _base_request(image=None, extensions={"poolRef": "ml-pool"})
    with pytest.raises(UnsupportedFieldError) as exc_info:
        map_create_request(request, "sbx-1", "ns-1", now=NOW)
    assert exc_info.value.field == "image"


def test_map_create_request_rejects_null_timeout():
    request = _base_request(timeout=None)
    with pytest.raises(UnsupportedFieldError) as exc_info:
        map_create_request(request, "sbx-1", "ns-1", now=NOW)
    assert exc_info.value.field == "timeout"


def test_map_create_request_rejects_null_env_value():
    request = _base_request(env={"EMPTY": None})
    with pytest.raises(UnsupportedFieldError) as exc_info:
        map_create_request(request, "sbx-1", "ns-1", now=NOW)
    assert exc_info.value.field == "env"


def test_map_create_request_rejects_unknown_extension_key():
    request = _base_request(extensions={"bootstrap.execd.isolation": "per-slot"})
    with pytest.raises(UnsupportedFieldError) as exc_info:
        map_create_request(request, "sbx-1", "ns-1", now=NOW)
    assert "extensions" in exc_info.value.field


@pytest.mark.parametrize(
    "metadata",
    [
        {"team.io/project": "agents"},  # dots and slashes are not DNS labels
        {"Team": "agents"},  # uppercase is not a DNS label
        {"a" * 64: "v"},  # key too long
        {"ok-key": "bad value!"},  # value with spaces
        {"ok-key": "v" * 64},  # value too long
    ],
)
def test_map_create_request_rejects_non_label_metadata(metadata):
    request = _base_request(metadata=metadata)
    with pytest.raises(UnsupportedFieldError) as exc_info:
        map_create_request(request, "sbx-1", "ns-1", now=NOW)
    assert exc_info.value.field == "metadata"


def test_map_create_request_accepts_label_compliant_metadata():
    request = _base_request(
        metadata={"team": "agents", "region-us-east-1": "prod"},
        env={"K": "v"},
    )
    create = map_create_request(request, "sbx-1", "ns-1", now=NOW)
    assert create.metadata["team"] == "agents"
    assert create.metadata["region-us-east-1"] == "prod"


def test_map_create_request_reuses_absolute_expiry_on_remap():
    first = map_create_request(_base_request(), "sbx-1", "ns-1", now=NOW)
    retry = map_create_request(
        _base_request(),
        "sbx-1",
        "ns-1",
        now=datetime(2026, 8, 18, 13, 0, 0, tzinfo=timezone.utc),
        expires_at_unix_seconds=first.expires_at_unix_seconds,
    )
    assert retry.expires_at_unix_seconds == first.expires_at_unix_seconds == EXPECTED_EXPIRY


def test_map_create_request_accepts_matching_pool_resources():
    request = _base_request(resource_limits=ResourceLimits(root={"cpu": "500m", "memory": "512Mi"}))
    create = map_create_request(
        request,
        "sbx-1",
        "ns-1",
        now=NOW,
        pool_resources={"cpu": "500m", "memory": "512Mi", "pids": "256"},
    )
    assert create.image == "python:3.11"


def test_map_create_request_rejects_mismatched_pool_resources():
    request = _base_request(resource_limits=ResourceLimits(root={"cpu": "1"}))
    with pytest.raises(UnsupportedFieldError) as exc_info:
        map_create_request(
            request,
            "sbx-1",
            "ns-1",
            now=NOW,
            pool_resources={"cpu": "500m", "memory": "512Mi"},
        )
    assert exc_info.value.field == "resourceLimits"


def test_map_create_request_rejects_undefinted_pool_resource_key():
    request = _base_request(resource_limits=ResourceLimits(root={"gpu": "1"}))
    with pytest.raises(UnsupportedFieldError) as exc_info:
        map_create_request(request, "sbx-1", "ns-1", now=NOW, pool_resources={"cpu": "500m"})
    assert exc_info.value.field == "resourceLimits"


def test_map_create_request_compares_quantities_canonically():
    # Same quantity expressed differently must not be rejected.
    request = _base_request(resource_limits=ResourceLimits(root={"cpu": "0.5", "memory": "1Gi"}))
    create = map_create_request(
        request,
        "sbx-1",
        "ns-1",
        now=NOW,
        pool_resources={"cpu": "500m", "memory": "1024Mi", "pids": "256"},
    )
    assert create.image == "python:3.11"


def test_map_create_request_skips_pool_check_when_profile_unknown():
    request = _base_request(resource_limits=ResourceLimits(root={"cpu": "1"}))
    create = map_create_request(request, "sbx-1", "ns-1", now=NOW)
    assert create.image == "python:3.11"


# -- status mapping -----------------------------------------------------------


def _info(
    *,
    runtime_state=pb2.RUNTIME_STATE_READY,
    data_plane_state=pb2.DATA_PLANE_STATE_READY,
    ready=False,
):
    return pb2.SandboxInfo(
        identity=pb2.SandboxIdentity(uid="uid-1", name="sbx-1", namespace="ns-1"),
        runtime=pb2.RuntimeInfo(state=runtime_state),
        data_plane=pb2.DataPlaneInfo(state=data_plane_state),
        ready=ready,
    )


@pytest.mark.parametrize(
    "runtime,data_plane,ready,expected",
    [
        (pb2.RUNTIME_STATE_READY, pb2.DATA_PLANE_STATE_READY, True, "Running"),
        (pb2.RUNTIME_STATE_READY, pb2.DATA_PLANE_STATE_READY, False, "Pending"),
        (pb2.RUNTIME_STATE_READY, pb2.DATA_PLANE_STATE_PENDING, False, "Pending"),
        (pb2.RUNTIME_STATE_PENDING, pb2.DATA_PLANE_STATE_UNSPECIFIED, False, "Pending"),
        (pb2.RUNTIME_STATE_CREATING, pb2.DATA_PLANE_STATE_UNSPECIFIED, False, "Pending"),
        (pb2.RUNTIME_STATE_STOPPING, pb2.DATA_PLANE_STATE_UNSPECIFIED, False, "Stopping"),
        (pb2.RUNTIME_STATE_READY, pb2.DATA_PLANE_STATE_DRAINING, False, "Stopping"),
        (pb2.RUNTIME_STATE_STOPPED, pb2.DATA_PLANE_STATE_UNSPECIFIED, False, "Terminated"),
        (pb2.RUNTIME_STATE_STOPPED, pb2.DATA_PLANE_STATE_DRAINING, False, "Terminated"),
        (pb2.RUNTIME_STATE_FAILED, pb2.DATA_PLANE_STATE_DRAINING, False, "Failed"),
        (pb2.RUNTIME_STATE_FAILED, pb2.DATA_PLANE_STATE_UNSPECIFIED, False, "Failed"),
        (pb2.RUNTIME_STATE_UNAVAILABLE, pb2.DATA_PLANE_STATE_UNSPECIFIED, False, "Failed"),
        (pb2.RUNTIME_STATE_READY, pb2.DATA_PLANE_STATE_FAILED, False, "Failed"),
        (pb2.RUNTIME_STATE_READY, pb2.DATA_PLANE_STATE_UNAVAILABLE, False, "Failed"),
        (
            pb2.RUNTIME_STATE_UNSPECIFIED,
            pb2.DATA_PLANE_STATE_UNSPECIFIED,
            False,
            "Pending",
        ),
    ],
)
def test_map_state_matrix(runtime, data_plane, ready, expected):
    assert (
        map_state(_info(runtime_state=runtime, data_plane_state=data_plane, ready=ready))
        == expected
    )


def test_map_state_accounts_for_components_bindings_and_ready_shortcut():
    component_failed = _info(ready=True)
    component_failed.infra_components.add(name="execd", state=pb2.INFRA_COMPONENT_STATE_FAILED)
    explicitly_ready = _info(
        runtime_state=pb2.RUNTIME_STATE_PENDING,
        data_plane_state=pb2.DATA_PLANE_STATE_PENDING,
    )
    explicitly_ready.ready = True

    assert map_state(component_failed) == "Failed"
    assert map_state(explicitly_ready) == "Running"


def test_map_state_transient_binding_failure_with_live_runtime():
    # Bindings (egress policy delivery) report Failed for their whole
    # delivery window and retry; the data plane is Pending/Publishing while
    # the sandbox converges. Mapping that window to Failed tells clients to
    # delete a sandbox whose execd already answers, so a failed binding only
    # fails the aggregate once the data plane left convergence.
    converging_binding_failed = _info(
        runtime_state=pb2.RUNTIME_STATE_CREATING,
        data_plane_state=pb2.DATA_PLANE_STATE_PENDING,
    )
    converging_binding_failed.action_bindings.add(handler="egress", state=pb2.ACTION_STATE_FAILED)
    assert map_state(converging_binding_failed) == "Pending"

    publishing_binding_failed = _info(
        runtime_state=pb2.RUNTIME_STATE_READY,
        data_plane_state=pb2.DATA_PLANE_STATE_PUBLISHING,
    )
    publishing_binding_failed.action_bindings.add(
        handler="egress", state=pb2.ACTION_STATE_FAILED
    )
    assert map_state(publishing_binding_failed) == "Pending"

    # Data plane settled but the binding still failed: a running-time
    # failure of the delivered policy, surfaced as Failed.
    settled_binding_failed = _info(ready=True)
    settled_binding_failed.action_bindings.add(
        handler="egress", state=pb2.ACTION_STATE_FAILED
    )
    assert map_state(settled_binding_failed) == "Failed"


@pytest.mark.parametrize(
    "runtime,data_plane,ready,expected",
    [
        (pb2.RUNTIME_STATE_READY, pb2.DATA_PLANE_STATE_READY, True, None),
        (pb2.RUNTIME_STATE_UNAVAILABLE, pb2.DATA_PLANE_STATE_READY, False, "RuntimeUnavailable"),
        (pb2.RUNTIME_STATE_READY, pb2.DATA_PLANE_STATE_UNAVAILABLE, False, "DataPlaneUnavailable"),
        (pb2.RUNTIME_STATE_FAILED, pb2.DATA_PLANE_STATE_READY, False, "Failed"),
    ],
)
def test_map_reason(runtime, data_plane, ready, expected):
    assert (
        map_reason(_info(runtime_state=runtime, data_plane_state=data_plane, ready=ready))
        == expected
    )


# -- OSEP-0026: gated template resource forwarding ------------------------------


def _template_request(**overrides):
    from opensandbox_server.api.schema import CreateSandboxRequest

    payload = {"template_id": "tpl-1", "timeout": 3600}
    payload.update(overrides)
    return CreateSandboxRequest(**payload)


def _map_template(request, **kwargs):
    defaults = dict(
        sandbox_id="sbx-1",
        namespace="ns-1",
        image_ref="oci://reg/t@sha256:" + "a" * 64,
        entrypoint=["tail", "-f", "/dev/null"],
        now=NOW,
    )
    defaults.update(kwargs)
    return map_template_create_request(request, **defaults)


def test_map_template_omits_resources_by_default():
    """Regression guard: no gate inputs → field unset, identical to today."""
    create = _map_template(_template_request())
    assert not create.HasField("resources")


def test_map_template_omits_resources_when_flag_off_even_on_resource_aware_pool():
    create = _map_template(
        _template_request(),
        template_machine={"cpu": "2", "memory": "1Gi"},
        pool_is_resource_aware=True,
        forward_resources=False,
    )
    assert not create.HasField("resources")


def test_map_template_omits_resources_on_legacy_pool_even_with_flag_on():
    create = _map_template(
        _template_request(),
        template_machine={"cpu": "2", "memory": "1Gi"},
        pool_is_resource_aware=False,
        forward_resources=True,
    )
    assert not create.HasField("resources")


def test_map_template_forwards_machine_verbatim_when_gate_passes():
    create = _map_template(
        _template_request(),
        template_machine={"cpu": "2", "memory": "1Gi"},
        pool_is_resource_aware=True,
        forward_resources=True,
    )
    assert create.HasField("resources")
    assert create.resources.limits.cpu == "2"
    assert create.resources.limits.memory == "1Gi"


def test_map_template_rejects_machine_below_fastpath_minimum():
    with pytest.raises(UnsupportedFieldError, match="10m"):
        _map_template(
            _template_request(),
            template_machine={"cpu": "5m", "memory": "1Gi"},
            pool_is_resource_aware=True,
            forward_resources=True,
        )


@pytest.mark.parametrize(
    "machine",
    [None, {}, {"cpu": "abc", "memory": "1Gi"}, {"cpu": "0", "memory": "1Gi"}],
)
def test_map_template_rejects_invalid_machine(machine):
    with pytest.raises(UnsupportedFieldError):
        _map_template(
            _template_request(),
            template_machine=machine,
            pool_is_resource_aware=True,
            forward_resources=True,
        )


def test_map_template_remap_is_byte_identical_with_same_inputs():
    """Idempotency: retry with the same request_id/expiry/machine serializes
    to the identical CreateSandboxRequest (limits are CRD-immutable)."""
    request = _template_request()
    first = _map_template(
        request,
        template_machine={"cpu": "2", "memory": "1Gi"},
        pool_is_resource_aware=True,
        forward_resources=True,
    )
    second = _map_template(
        request,
        sandbox_id=first.request_id,
        expires_at_unix_seconds=first.expires_at_unix_seconds,
        template_machine={"cpu": "2", "memory": "1Gi"},
        pool_is_resource_aware=True,
        forward_resources=True,
    )
    assert second.request_id == first.request_id
    assert second.expires_at_unix_seconds == EXPECTED_EXPIRY
    assert second.SerializeToString() == first.SerializeToString()
