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

from __future__ import annotations

import math
import re
from typing import Any, Dict, List, Optional

from opensandbox_server.api.schema import PlatformSpec
from opensandbox_server.services.k8s.provider_common import DEFAULT_ENTRYPOINT
from opensandbox_server.services.windows_common import (
    inject_windows_resource_limits_env,
    inject_windows_user_ports,
    validate_windows_resource_limits,
)

WINDOWS_OEM_VOLUME_NAME = "opensandbox-win-oem"
WINDOWS_KVM_VOLUME_NAME = "opensandbox-win-kvm"
WINDOWS_TUN_VOLUME_NAME = "opensandbox-win-tun"
WINDOWS_STORAGE_VOLUME_NAME = "opensandbox-win-storage"
WINDOWS_PROFILE_DEFAULT_USER_PORTS = ["44772", "8080", "3389/tcp", "3389/udp", "8006/tcp"]
# Extra memory overhead (in Gi) reserved for QEMU process on top of guest RAM.
WINDOWS_QEMU_MEMORY_OVERHEAD_GI = 2
_SIZE_PATTERN = re.compile(r"^\s*(\d+)\s*([a-zA-Z]*)\s*$")


def is_windows_profile(platform: Optional[PlatformSpec]) -> bool:
    return bool(platform and platform.os == "windows")


def validate_windows_profile_resource_limits(resource_limits: dict[str, str]) -> None:
    validate_windows_resource_limits(resource_limits or {})


def build_windows_profile_env(
    env: dict[str, str],
    resource_limits: dict[str, str],
) -> list[dict[str, str]]:
    env_items = [f"{key}={value}" for key, value in env.items()]
    env_items = inject_windows_resource_limits_env(env_items, resource_limits or {})
    env_items = inject_windows_user_ports(env_items, WINDOWS_PROFILE_DEFAULT_USER_PORTS)

    result: list[dict[str, str]] = []
    for item in env_items:
        if "=" not in item:
            continue
        key, value = item.split("=", 1)
        result.append({"name": key, "value": value})
    return result


def apply_windows_profile_overrides(
    pod_spec: Dict[str, Any],
    entrypoint: List[str],
    env: Dict[str, str],
    resource_limits: Dict[str, str],
    disable_ipv6_for_egress: bool = False,
) -> None:
    """
    Patch the generic BatchSandbox pod spec for windows profile semantics.
    """
    windows_env = build_windows_profile_env(env, resource_limits)
    init_containers = pod_spec.get("initContainers")
    containers = pod_spec.get("containers")
    if not isinstance(init_containers, list) or not init_containers:
        return
    if not isinstance(containers, list) or not containers:
        return

    init_container = init_containers[0]
    init_script = (
        "cp ./install.bat /oem/install.bat && "
        "cp ./execd.exe /oem/execd.exe && "
        "chmod 0644 /oem/install.bat /oem/execd.exe"
    )
    if disable_ipv6_for_egress:
        init_container["args"] = [f"set -e; echo 1 > /proc/sys/net/ipv6/conf/all/disable_ipv6 && {init_script}"]
        init_container["securityContext"] = {"privileged": True}
    else:
        init_container["args"] = [init_script]
        init_container.pop("securityContext", None)
    _merge_volume_mounts(
        init_container,
        [{"name": WINDOWS_OEM_VOLUME_NAME, "mountPath": "/oem"}],
    )

    main_container = containers[0]
    # Entrypoint handling for Windows profile:
    # - If user provides a custom entrypoint, use it as container command
    #   (e.g. for ENI network hack or other custom startup logic).
    # - If no entrypoint or the SDK default, remove command to use image
    #   ENTRYPOINT (dockur/windows starts QEMU via /run/entry.sh).
    if entrypoint and entrypoint != DEFAULT_ENTRYPOINT:
        main_container["command"] = entrypoint
    else:
        main_container.pop("command", None)
    main_container.pop("args", None)
    main_container["env"] = windows_env if windows_env else None
    # Set pod resources from resource_limits for proper K8s scheduling.
    # Memory includes overhead for the QEMU process itself.
    if resource_limits:
        limits: Dict[str, str] = {}
        if resource_limits.get("cpu"):
            limits["cpu"] = resource_limits["cpu"]
        if resource_limits.get("memory"):
            limits["memory"] = _memory_with_qemu_overhead(resource_limits["memory"])
        if limits:
            main_container["resources"] = {
                "limits": limits,
                "requests": dict(limits),
            }
        else:
            main_container.pop("resources", None)
    else:
        main_container.pop("resources", None)
    security_context = main_container.setdefault("securityContext", {})
    security_context["privileged"] = True
    capabilities = security_context.setdefault("capabilities", {})
    drop = capabilities.get("drop")
    if isinstance(drop, list):
        capabilities["drop"] = [cap for cap in drop if cap != "NET_ADMIN"]
        if not capabilities["drop"]:
            capabilities.pop("drop", None)
    add = capabilities.setdefault("add", [])
    for cap in ("NET_ADMIN", "NET_RAW"):
        if cap not in add:
            add.append(cap)
    _merge_volume_mounts(
        main_container,
        [
            {"name": WINDOWS_OEM_VOLUME_NAME, "mountPath": "/oem"},
            {"name": WINDOWS_KVM_VOLUME_NAME, "mountPath": "/dev/kvm"},
            {"name": WINDOWS_TUN_VOLUME_NAME, "mountPath": "/dev/net/tun"},
            {"name": WINDOWS_STORAGE_VOLUME_NAME, "mountPath": "/storage"},
        ],
    )

    _merge_volumes(
        pod_spec,
        [
            {"name": WINDOWS_OEM_VOLUME_NAME, "emptyDir": {}},
            {
                "name": WINDOWS_KVM_VOLUME_NAME,
                "hostPath": {"path": "/dev/kvm", "type": "CharDevice"},
            },
            {
                "name": WINDOWS_TUN_VOLUME_NAME,
                "hostPath": {"path": "/dev/net/tun", "type": "CharDevice"},
            },
            {"name": WINDOWS_STORAGE_VOLUME_NAME, "emptyDir": {}},
        ],
    )

    # dockur/windows relies on container restart to complete multi-phase
    # installation (first boot installs from ISO, second boot runs from disk).
    pod_spec["restartPolicy"] = "Always"


def apply_windows_profile_arch_selector(
    pod_spec: Dict[str, Any],
    template_spec: Dict[str, Any],
    platform: Optional[PlatformSpec],
) -> None:
    """
    Apply platform.arch constraint for windows profile pods.

    We intentionally avoid forcing kubernetes.io/os=windows for this profile,
    but still honor arch constraints from API requests and fail early on
    template conflicts.
    """
    if platform is None:
        return

    requested_arch = platform.arch
    template_selector = template_spec.get("nodeSelector", {})
    if not isinstance(template_selector, dict):
        template_selector = {}

    existing_arch = template_selector.get("kubernetes.io/arch")
    if existing_arch is not None and existing_arch != requested_arch:
        raise ValueError(
            "platform conflict with template nodeSelector: 'kubernetes.io/arch' "
            f"is '{existing_arch}', request expects '{requested_arch}'."
        )

    if not _template_allows_arch(template_spec, requested_arch):
        raise ValueError(
            "platform conflict with template nodeAffinity: required node affinity "
            f"does not allow requested architecture '{requested_arch}'."
        )

    node_selector = pod_spec.setdefault("nodeSelector", {})
    if not isinstance(node_selector, dict):
        node_selector = {}
        pod_spec["nodeSelector"] = node_selector
    node_selector["kubernetes.io/arch"] = requested_arch


def _merge_volume_mounts(container: Dict[str, Any], mounts_to_add: List[Dict[str, str]]) -> None:
    mounts = container.setdefault("volumeMounts", [])
    if not isinstance(mounts, list):
        mounts = []
        container["volumeMounts"] = mounts
    existing_names = {item.get("name") for item in mounts if isinstance(item, dict)}
    for mount in mounts_to_add:
        name = mount.get("name")
        if not name or name in existing_names:
            continue
        mounts.append(mount)
        existing_names.add(name)


def _memory_with_qemu_overhead(memory_value: str) -> str:
    """Add QEMU process overhead to guest memory for K8s pod resource limits.

    Parses the guest RAM value (e.g. '8G', '16Gi') and adds
    WINDOWS_QEMU_MEMORY_OVERHEAD_GI. Returns a Gi-suffixed string suitable
    for Kubernetes resource quantities.
    """
    match = _SIZE_PATTERN.match(memory_value)
    if not match:
        return memory_value
    amount = int(match.group(1))
    unit = (match.group(2) or "").lower()
    if unit in {"g", "gi", "gb"}:
        total_gi = amount + WINDOWS_QEMU_MEMORY_OVERHEAD_GI
    elif unit in {"m", "mi", "mb"}:
        total_gi = math.ceil(amount / 1024) + WINDOWS_QEMU_MEMORY_OVERHEAD_GI
    elif unit in {"t", "ti", "tb"}:
        total_gi = amount * 1024 + WINDOWS_QEMU_MEMORY_OVERHEAD_GI
    else:
        return memory_value
    return f"{total_gi}Gi"


def _merge_volumes(pod_spec: Dict[str, Any], volumes_to_add: List[Dict[str, Any]]) -> None:
    volumes = pod_spec.setdefault("volumes", [])
    if not isinstance(volumes, list):
        volumes = []
        pod_spec["volumes"] = volumes
    existing_names = {item.get("name") for item in volumes if isinstance(item, dict)}
    for volume in volumes_to_add:
        name = volume.get("name")
        if not name or name in existing_names:
            continue
        volumes.append(volume)
        existing_names.add(name)


def _template_allows_arch(template_spec: Dict[str, Any], requested_arch: str) -> bool:
    affinity = template_spec.get("affinity", {})
    if not isinstance(affinity, dict):
        return True

    node_affinity = affinity.get("nodeAffinity", {})
    if not isinstance(node_affinity, dict):
        return True

    required = node_affinity.get("requiredDuringSchedulingIgnoredDuringExecution", {})
    if not isinstance(required, dict):
        return True

    terms = required.get("nodeSelectorTerms", [])
    if not isinstance(terms, list) or not terms:
        return True

    return any(_arch_term_satisfiable(term, requested_arch) for term in terms if isinstance(term, dict))


def _arch_term_satisfiable(term: Dict[str, Any], requested_arch: str) -> bool:
    expressions = term.get("matchExpressions", [])
    if not isinstance(expressions, list):
        return True

    for expr in expressions:
        if not isinstance(expr, dict):
            continue
        if expr.get("key") != "kubernetes.io/arch":
            continue
        operator = expr.get("operator")
        values = expr.get("values", [])
        if not isinstance(values, list):
            values = []

        if operator == "In" and requested_arch not in values:
            return False
        if operator == "NotIn" and requested_arch in values:
            return False
        if operator == "DoesNotExist":
            return False

    return True
