# Copyright 2026 Alibaba Group Holding Ltd.
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
