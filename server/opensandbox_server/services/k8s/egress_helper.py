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

from opensandbox_server.services.constants import (
    EGRESS_MODE_ENV,
    EGRESS_RULES_ENV,
    OPEN_SANDBOX_EGRESS_AUTH_HEADER,
    OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT,
    OPENSANDBOX_EGRESS_SANDBOX_ID,
    OPENSANDBOX_EGRESS_TOKEN,
    OPENSANDBOX_RUNTIME_MOUNT_PATH,
    OPENSANDBOX_RUNTIME_VOLUME_NAME,
)
from opensandbox_server.services.k8s.workload_provider import EgressWorkloadSettings


def prep_execd_init_for_egress(exec_install_script: str) -> tuple[str, Dict[str, Any]]:
    """
    Prepare execd init when ``egress.disable_ipv6`` is true: disable IPv6 in the Pod netns, then install.

    Writes ``/proc/sys/.../disable_ipv6`` (no ``sysctl`` binary required). The returned
    security context dict must be applied to the execd init container (typically via
    ``build_security_context_from_dict`` in ``security_context``).

    Returns:
        ``(prefixed_shell_script, {"privileged": True})``
    """
    script = f"set -e; echo 1 > /proc/sys/net/ipv6/conf/all/disable_ipv6 && {exec_install_script}"
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
    egress_settings: Optional[EgressWorkloadSettings] = None,
    sandbox_id: Optional[str] = None,
) -> None:
    """
    Append the egress sidecar to ``containers``. When ``egress.disable_ipv6`` is enabled,
    IPv6 is handled in execd init (``prep_execd_init_for_egress``); Pod-level sysctls are not modified.

    ``sandbox_id`` is injected as ``OPENSANDBOX_EGRESS_SANDBOX_ID`` when provided.
    """
    if egress_settings is None:
        return

    policy_payload = json.dumps(
        egress_settings.network_policy.model_dump(by_alias=True, exclude_none=True)
    )

    env: List[Dict[str, str]] = [
        {"name": EGRESS_RULES_ENV, "value": policy_payload},
        {"name": EGRESS_MODE_ENV, "value": egress_settings.mode},
    ]
    if sandbox_id:
        env.append({"name": OPENSANDBOX_EGRESS_SANDBOX_ID, "value": sandbox_id})
    if egress_settings.credential_proxy_enabled:
        env.append({"name": OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT, "value": "true"})
    if egress_settings.auth_token:
        env.append({"name": OPENSANDBOX_EGRESS_TOKEN, "value": egress_settings.auth_token})
    if egress_settings.env:
        for name, value in egress_settings.env.items():
            if (
                egress_settings.credential_proxy_enabled
                and name == OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT
            ):
                continue
            env.append({"name": name, "value": value or ""})

    sidecar: Dict[str, Any] = {
        "name": "egress",
        "image": egress_settings.image,
        "env": env,
        "securityContext": {
            "capabilities": {"add": ["NET_ADMIN"]},
        },
        "ports": [{"name": "egress-api", "containerPort": 18080}],
        "readinessProbe": {
            "httpGet": {
                "path": "/healthz",
                "port": 18080,
            },
            "periodSeconds": 1,
            "failureThreshold": 30,
        },
    }
    sidecar["volumeMounts"] = [
        {
            "name": OPENSANDBOX_RUNTIME_VOLUME_NAME,
            "mountPath": OPENSANDBOX_RUNTIME_MOUNT_PATH,
        }
    ]
    resources = {}
    if egress_settings.resource_requests:
        resources["requests"] = egress_settings.resource_requests
    if egress_settings.resource_limits:
        resources["limits"] = egress_settings.resource_limits
    if resources:
        sidecar["resources"] = resources
    if egress_settings.auth_token:
        sidecar["readinessProbe"]["httpGet"]["httpHeaders"] = [
            {
                "name": OPEN_SANDBOX_EGRESS_AUTH_HEADER,
                "value": egress_settings.auth_token,
            }
        ]
    containers.append(sidecar)
