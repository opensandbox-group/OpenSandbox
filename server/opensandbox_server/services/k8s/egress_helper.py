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

"""
Egress sidecar helpers for Kubernetes pod specs.

Public entry points: ``prep_execd_init_for_egress``, ``build_egress_bootstrap_init_container``,
``build_security_context_for_sandbox_container``, ``apply_egress_to_spec``,
``validate_forward_mode_pod_spec``. SecurityContext dict ↔ V1 conversion lives in ``security_context``.
"""

import json
import shlex
from typing import Any, Dict, List, Optional

from opensandbox_server.config import EGRESS_MODE_FORWARD
from opensandbox_server.services.constants import (
    EGRESS_MODE_ENV,
    EGRESS_RESOLVER_MOUNT_PATH,
    EGRESS_RESOLVER_VOLUME_NAME,
    EGRESS_RULES_ENV,
    EGRESS_UPSTREAM_EXTRA_CA_PATH,
    EGRESS_UPSTREAM_EXTRA_CA_SECRET_KEY,
    EGRESS_UPSTREAM_EXTRA_CA_VOLUME_NAME,
    OTEL_EXPORTER_OTLP_ENDPOINT,
    OPEN_SANDBOX_EGRESS_AUTH_HEADER,
    OPENSANDBOX_EGRESS_DNS_UPSTREAM,
    OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT,
    OPENSANDBOX_EGRESS_SANDBOX_ID,
    OPENSANDBOX_EGRESS_SANDBOX_NAMESPACE,
    OPENSANDBOX_EGRESS_TENANT_NAME,
    OPENSANDBOX_EGRESS_TOKEN,
    OPENSANDBOX_RUNTIME_MOUNT_PATH,
    OPENSANDBOX_RUNTIME_VOLUME_NAME,
)
from opensandbox_server.services.helpers import (
    enforcement_egress_env,
    upstream_proxy_egress_env,
)
from opensandbox_server.services.k8s.workload_provider import EgressWorkloadSettings

_IPV6_DISABLE_PATH = "/proc/sys/net/ipv6/conf/all/disable_ipv6"

# Installed by the egress-init initContainer before any container in
# spec.containers starts (OSEP-0030 R1). First snapshots kubelet's /etc/hosts
# and /etc/resolv.conf into an egress-only emptyDir (mounted read-only by
# egress and never mounted by the workload), so a root workload cannot poison
# gateway resolution through the Pod's shared writable /etc/hosts or
# /etc/resolv.conf before or after egress starts. Then installs the bootstrap
# default-deny fence, which the egress sidecar atomically replaces once ready.
_FORWARD_BOOTSTRAP_FENCE_SCRIPT = (
    "set -eu\n"
    f"cp /etc/hosts {EGRESS_RESOLVER_MOUNT_PATH}/hosts\n"
    f"cp /etc/resolv.conf {EGRESS_RESOLVER_MOUNT_PATH}/resolv.conf\n"
    f"chmod 0444 {EGRESS_RESOLVER_MOUNT_PATH}/hosts {EGRESS_RESOLVER_MOUNT_PATH}/resolv.conf\n"
    "nft -f - <<'EOF'\n"
    "table inet opensandbox\n"
    "delete table inet opensandbox\n"
    "table inet opensandbox {\n"
    "  chain egress {\n"
    "    type filter hook output priority 0; policy drop;\n"
    "    oifname \"lo\" accept\n"
    "  }\n"
    "}\n"
    "EOF\n"
)

_FORBIDDEN_FORWARD_CAPS = frozenset({"NET_ADMIN", "NET_RAW"})
_FORBIDDEN_FORWARD_WORKLOAD_VOLUMES = frozenset(
    {EGRESS_RESOLVER_VOLUME_NAME, EGRESS_UPSTREAM_EXTRA_CA_VOLUME_NAME}
)


def prep_execd_init_for_egress(exec_install_script: str) -> tuple[str, Dict[str, Any]]:
    """
    Prepare execd init when ``egress.disable_ipv6`` is true: disable IPv6 in the Pod netns, then install.

    Writes ``/proc/sys/.../disable_ipv6`` when that path exists (no ``sysctl`` binary
    required). A missing path is valid for an IPv4-only Pod netns, while a write failure
    remains fatal. The returned security context dict must be applied to the execd init
    container (typically via ``build_security_context_from_dict`` in ``security_context``).

    Returns:
        ``(prefixed_shell_script, {"privileged": True})``
    """
    ipv6_disable_path = shlex.quote(_IPV6_DISABLE_PATH)
    script = (
        f"set -e; if [ -e {ipv6_disable_path} ]; then "
        f"echo 1 > {ipv6_disable_path}; fi; {exec_install_script}"
    )
    return script, {"privileged": True}


def build_egress_bootstrap_init_container(
    egress_settings: EgressWorkloadSettings,
) -> Dict[str, Any]:
    """Build the forward-mode bootstrap default-deny nft fence initContainer.

    Runs using ``egress_settings.image`` (which already carries ``nft``) before
    any container in ``spec.containers`` starts, installing ``table inet
    opensandbox`` with loopback-only accept and default drop. The egress
    sidecar atomically replaces that table once initialized.
    """
    init_container: Dict[str, Any] = {
        "name": "egress-init",
        "image": egress_settings.image,
        "command": ["/bin/sh", "-c"],
        "args": [_FORWARD_BOOTSTRAP_FENCE_SCRIPT],
        "securityContext": {
            "capabilities": {"add": ["NET_ADMIN"]},
        },
        "volumeMounts": [
            {
                "name": EGRESS_RESOLVER_VOLUME_NAME,
                "mountPath": EGRESS_RESOLVER_MOUNT_PATH,
            }
        ],
    }
    resources = {}
    if egress_settings.resource_requests:
        resources["requests"] = egress_settings.resource_requests
    if egress_settings.resource_limits:
        resources["limits"] = egress_settings.resource_limits
    if resources:
        init_container["resources"] = resources
    return init_container


def build_security_context_for_sandbox_container(
    has_network_policy: bool,
    *,
    drop_net_raw: bool = False,
) -> Dict[str, Any]:
    """
    Security context dict for the main sandbox container.

    When network policy is enabled, drops ``NET_ADMIN`` so only the egress sidecar can
    mutate network stack state. Forward mode (``drop_net_raw=True``) also drops
    ``NET_RAW`` so the workload cannot set the forwarder's ``SO_MARK`` (OSEP-0030 R8).
    """
    if not has_network_policy:
        return {}

    drop = ["NET_ADMIN", "NET_RAW"] if drop_net_raw else ["NET_ADMIN"]
    return {
        "capabilities": {
            "drop": drop,
        },
    }


def _normalize_cap(cap: str) -> str:
    return cap.strip().upper().removeprefix("CAP_")


def validate_forward_mode_pod_spec(pod_spec: Dict[str, Any]) -> None:
    """Enforce OSEP-0030 R8 invariants on the final merged Pod spec.

    Rejects pod templates that enable ``shareProcessNamespace``, ``hostPID``,
    or ``hostNetwork``, or grant ``privileged`` / ``NET_ADMIN`` / ``NET_RAW`` /
    ``ALL`` (or fail to drop ``NET_ADMIN`` and ``NET_RAW``) on any non-egress
    container.
    """
    if pod_spec.get("shareProcessNamespace"):
        raise ValueError(
            'Pod template cannot enable shareProcessNamespace when egress.mode = "forward"'
        )
    if pod_spec.get("hostPID"):
        raise ValueError(
            'Pod template cannot enable hostPID when egress.mode = "forward"'
        )
    if pod_spec.get("hostNetwork"):
        raise ValueError(
            'Pod template cannot enable hostNetwork when egress.mode = "forward"'
        )

    for container in pod_spec.get("containers") or []:
        if not isinstance(container, dict) or container.get("name") == "egress":
            continue
        name = container.get("name") or "sandbox"
        sc = container.get("securityContext") or {}
        if not isinstance(sc, dict):
            sc = {}
        if sc.get("privileged"):
            raise ValueError(
                f"Container '{name}' cannot run privileged when egress.mode = \"forward\""
            )
        caps = sc.get("capabilities") or {}
        if not isinstance(caps, dict):
            caps = {}
        added = {_normalize_cap(c) for c in (caps.get("add") or []) if isinstance(c, str)}
        forbidden_added = added & (_FORBIDDEN_FORWARD_CAPS | {"ALL"})
        if forbidden_added:
            raise ValueError(
                f"Container '{name}' cannot add capabilities {sorted(forbidden_added)} "
                'when egress.mode = "forward"'
            )
        dropped = {_normalize_cap(c) for c in (caps.get("drop") or []) if isinstance(c, str)}
        if "ALL" not in dropped and not (_FORBIDDEN_FORWARD_CAPS <= dropped):
            raise ValueError(
                f"Container '{name}' must drop NET_ADMIN and NET_RAW "
                'when egress.mode = "forward"'
            )
        for mount in container.get("volumeMounts") or []:
            if isinstance(mount, dict) and mount.get("name") in _FORBIDDEN_FORWARD_WORKLOAD_VOLUMES:
                raise ValueError(
                    f"Container '{name}' cannot mount egress-internal volume "
                    f"'{mount.get('name')}' when egress.mode = \"forward\""
                )


def apply_egress_to_spec(
    containers: List[Dict[str, Any]],
    egress_settings: Optional[EgressWorkloadSettings] = None,
    sandbox_id: Optional[str] = None,
    pod_volumes: Optional[List[Dict[str, Any]]] = None,
    init_containers: Optional[List[Dict[str, Any]]] = None,
) -> None:
    """
    Append the egress sidecar to ``containers``. When ``egress.disable_ipv6`` is enabled,
    IPv6 is handled in execd init (``prep_execd_init_for_egress``); Pod-level sysctls are not modified.

    ``sandbox_id`` is injected as ``OPENSANDBOX_EGRESS_SANDBOX_ID`` when provided.
    ``egress.otlp_endpoint`` is injected as ``OTEL_EXPORTER_OTLP_ENDPOINT`` when configured.
    ``egress.enforcement`` and ``egress.redirect_backend`` are injected as the
    ``OPENSANDBOX_EGRESS_ENFORCEMENT_*`` / ``OPENSANDBOX_EGRESS_REDIRECT_BACKEND``
    variables when configured.
    When ``egress_settings.upstream_proxy.ca_secret_name`` is set, the CA Secret is
    projected onto ``pod_volumes`` (required, to avoid a dangling mount) and
    mounted read-only into the egress sidecar only.
    In forward mode, ``init_containers`` is required so the bootstrap default-deny
    nft fence initContainer is installed before any container in ``spec.containers`` starts.
    """
    if egress_settings is None:
        return

    is_forward = egress_settings.mode == EGRESS_MODE_FORWARD
    if is_forward:
        if init_containers is None:
            raise ValueError(
                "init_containers is required for egress forward mode bootstrap fence"
            )
        if pod_volumes is None:
            raise ValueError(
                "pod_volumes is required for egress forward mode resolver snapshot"
            )
        pod_volumes.append(
            {
                "name": EGRESS_RESOLVER_VOLUME_NAME,
                "emptyDir": {},
            }
        )
        init_containers.append(build_egress_bootstrap_init_container(egress_settings))

    env: List[Dict[str, str]] = []
    if egress_settings.network_policy is not None:
        policy_payload = json.dumps(
            egress_settings.network_policy.model_dump(by_alias=True, exclude_none=True)
        )
        env.append({"name": EGRESS_RULES_ENV, "value": policy_payload})
    env.append({"name": EGRESS_MODE_ENV, "value": egress_settings.mode})
    if egress_settings.otlp_endpoint:
        env.append(
            {"name": OTEL_EXPORTER_OTLP_ENDPOINT, "value": egress_settings.otlp_endpoint}
        )
    for name, value in upstream_proxy_egress_env(egress_settings.upstream_proxy).items():
        env.append({"name": name, "value": value})
    if is_forward and egress_settings.dns_upstreams:
        env.append(
            {
                "name": OPENSANDBOX_EGRESS_DNS_UPSTREAM,
                "value": ",".join(egress_settings.dns_upstreams),
            }
        )
    for name, value in enforcement_egress_env(
        egress_settings.enforcement, egress_settings.redirect_backend
    ).items():
        env.append({"name": name, "value": value})
    if sandbox_id:
        env.append({"name": OPENSANDBOX_EGRESS_SANDBOX_ID, "value": sandbox_id})
    if is_forward and egress_settings.sandbox_namespace:
        env.append(
            {
                "name": OPENSANDBOX_EGRESS_SANDBOX_NAMESPACE,
                "value": egress_settings.sandbox_namespace,
            }
        )
    if is_forward and egress_settings.tenant_name:
        env.append(
            {
                "name": OPENSANDBOX_EGRESS_TENANT_NAME,
                "value": egress_settings.tenant_name,
            }
        )
    if egress_settings.credential_proxy_enabled:
        env.append({"name": OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT, "value": "true"})
    if egress_settings.auth_token:
        env.append({"name": OPENSANDBOX_EGRESS_TOKEN, "value": egress_settings.auth_token})
    upstream_proxy = egress_settings.upstream_proxy
    ca_secret_name = upstream_proxy.ca_secret_name if upstream_proxy else None
    if ca_secret_name is not None:
        if pod_volumes is None:
            raise ValueError(
                "pod_volumes is required for upstream proxy CA Secret mounting"
            )
        pod_volumes.append(
            {
                "name": EGRESS_UPSTREAM_EXTRA_CA_VOLUME_NAME,
                "secret": {
                    "secretName": ca_secret_name,
                    "items": [
                        {
                            "key": EGRESS_UPSTREAM_EXTRA_CA_SECRET_KEY,
                            "path": EGRESS_UPSTREAM_EXTRA_CA_SECRET_KEY,
                        }
                    ],
                },
            }
        )
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
    if is_forward:
        sidecar["volumeMounts"].append(
            {
                "name": EGRESS_RESOLVER_VOLUME_NAME,
                "mountPath": EGRESS_RESOLVER_MOUNT_PATH,
                "readOnly": True,
            }
        )
    if ca_secret_name is not None:
        sidecar["volumeMounts"].append(
            {
                "name": EGRESS_UPSTREAM_EXTRA_CA_VOLUME_NAME,
                "mountPath": EGRESS_UPSTREAM_EXTRA_CA_PATH,
                "subPath": EGRESS_UPSTREAM_EXTRA_CA_SECRET_KEY,
                "readOnly": True,
            }
        )
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
