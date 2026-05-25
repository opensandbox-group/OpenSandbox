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

"""
Egress sidecar helpers for Kubernetes pod specs.

Public entry points: ``prep_execd_init_for_egress``, ``build_security_context_for_sandbox_container``,
``apply_egress_to_spec``. SecurityContext dict ↔ V1 conversion lives in ``security_context``.
"""

import json
from typing import Any, Dict, List, Optional

from opensandbox_server.api.schema import NetworkPolicy
from opensandbox_server.config import EGRESS_MODE_DNS
from opensandbox_server.services.constants import (
    EGRESS_MODE_ENV,
    EGRESS_RULES_ENV,
    OPENSANDBOX_EGRESS_TOKEN,
)


def prep_execd_init_for_egress(exec_install_script: str) -> tuple[str, Dict[str, Any]]:
    """
    Prepare execd init when ``egress.disable_ipv6`` is true: disable IPv6 in the Pod netns, then install.

    Writes ``/proc/sys/.../disable_ipv6`` (no ``sysctl`` binary required). The returned
    security context dict must be applied to the execd init container (typically via
    ``build_security_context_from_dict`` in ``security_context``).

    Returns:
        ``(prefixed_shell_script, {"privileged": True})``
    """
    script = (
        "set -e; "
        "echo 1 > /proc/sys/net/ipv6/conf/all/disable_ipv6 && "
        f"{exec_install_script}"
    )
    return script, {"privileged": True}


def build_security_context_for_sandbox_container(
    has_network_policy: bool,
) -> Dict[str, Any]:
    """
    Security context dict for the main sandbox container.

    When network policy is enabled, drops ``NET_ADMIN`` so only the egress sidecar can
    mutate network stack state.
    """
    if not has_network_policy:
        return {}

    return {
        "capabilities": {
            "drop": ["NET_ADMIN"],
        },
    }


def apply_egress_to_spec(
    containers: List[Dict[str, Any]],
    network_policy: Optional[NetworkPolicy],
    egress_image: Optional[str],
    egress_auth_token: Optional[str] = None,
    egress_mode: str = EGRESS_MODE_DNS,
) -> None:
    """
    Append the egress sidecar to ``containers``. When ``egress.disable_ipv6`` is enabled,
    IPv6 is handled in execd init (``prep_execd_init_for_egress``); Pod-level sysctls are not modified.
    """
    if not network_policy or not egress_image:
        return

    policy_payload = json.dumps(
        network_policy.model_dump(by_alias=True, exclude_none=True)
    )

    env: List[Dict[str, str]] = [
        {"name": EGRESS_RULES_ENV, "value": policy_payload},
        {"name": EGRESS_MODE_ENV, "value": egress_mode},
    ]
    if egress_auth_token:
        env.append({"name": OPENSANDBOX_EGRESS_TOKEN, "value": egress_auth_token})

    containers.append(
        {
            "name": "egress",
            "image": egress_image,
            "env": env,
            "securityContext": {
                "capabilities": {"add": ["NET_ADMIN"]},
            },
        }
    )
