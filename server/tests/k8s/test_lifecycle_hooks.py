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
from datetime import datetime, timezone

from opensandbox_server.api.schema import (
    IdlePolicy,
    LifecycleHook,
    PeriodicLifecycleHook,
    SandboxLifecycle,
)
from opensandbox_server.services.constants import OPENSANDBOX_LIFECYCLE
from opensandbox_server.services.k8s.create_helpers import (
    _build_create_workload_context,
)


def test_create_context_transports_lifecycle_as_reserved_execd_env(
    k8s_app_config,
    create_sandbox_request,
):
    create_sandbox_request.env = {"USER_ENV": "value"}
    create_sandbox_request.lifecycle = SandboxLifecycle(
        preStart=LifecycleHook(
            command=["/opt/hooks/restore.sh"],
            timeoutSeconds=30,
        ),
        periodic=[
            PeriodicLifecycleHook(
                name="checkpoint",
                schedule="*/5 * * * *",
                command=["/opt/hooks/checkpoint.sh"],
            )
        ],
    )

    context = _build_create_workload_context(
        k8s_app_config,
        create_sandbox_request,
        "sandbox-1",
        datetime.now(timezone.utc),
        lambda: "egress-token",
        lambda: "secure-token",
    )

    assert context.sandbox_env["USER_ENV"] == "value"
    assert json.loads(context.sandbox_env[OPENSANDBOX_LIFECYCLE]) == {
        "preStart": {
            "command": ["/opt/hooks/restore.sh"],
            "timeoutSeconds": 30,
        },
        "periodic": [
            {
                "name": "checkpoint",
                "schedule": "*/5 * * * *",
                "command": ["/opt/hooks/checkpoint.sh"],
            }
        ],
    }


def test_create_context_never_transports_idle_policy_to_execd(
    k8s_app_config,
    create_sandbox_request,
):
    """idlePolicy is control-plane state (OSEP-0024); hooks only on the wire."""
    create_sandbox_request.lifecycle = SandboxLifecycle(
        preStart=LifecycleHook(command=["/opt/hooks/restore.sh"]),
        idlePolicy=IdlePolicy(idleTimeoutSeconds=300, onIdle="pause"),
    )

    context = _build_create_workload_context(
        k8s_app_config,
        create_sandbox_request,
        "sandbox-1",
        datetime.now(timezone.utc),
        lambda: "egress-token",
        lambda: "secure-token",
    )

    assert "idlePolicy" not in json.loads(context.sandbox_env[OPENSANDBOX_LIFECYCLE])


def test_create_context_never_transports_renewal_policy_to_execd(
    k8s_app_config,
    create_sandbox_request,
):
    """renewalPolicy is control-plane state (OSEP-0009); hooks only on the wire."""
    create_sandbox_request.lifecycle = SandboxLifecycle(
        preStart=LifecycleHook(command=["/opt/hooks/restore.sh"]),
        renewalPolicy={"extendSeconds": 3600},
    )

    context = _build_create_workload_context(
        k8s_app_config,
        create_sandbox_request,
        "sandbox-1",
        datetime.now(timezone.utc),
        lambda: "egress-token",
        lambda: "secure-token",
    )

    assert "renewalPolicy" not in json.loads(context.sandbox_env[OPENSANDBOX_LIFECYCLE])


def test_create_context_skips_lifecycle_env_for_policy_only_lifecycle(
    k8s_app_config,
    create_sandbox_request,
):
    """A renewal-only lifecycle must not inject OPENSANDBOX_LIFECYCLE: an
    empty payload would overwrite hooks persisted in the image/snapshot."""
    create_sandbox_request.lifecycle = SandboxLifecycle(
        renewalPolicy={"extendSeconds": 3600},
    )

    context = _build_create_workload_context(
        k8s_app_config,
        create_sandbox_request,
        "sandbox-1",
        datetime.now(timezone.utc),
        lambda: "egress-token",
        lambda: "secure-token",
    )

    assert OPENSANDBOX_LIFECYCLE not in context.sandbox_env
