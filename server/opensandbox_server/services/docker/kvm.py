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

import os
import stat
from typing import Any, Final, Mapping

from fastapi import HTTPException, status

from opensandbox_server.services.constants import SandboxErrorCodes

KVM_DEVICE_PATH: Final = "/dev/kvm"
KVM_DEVICE_MAPPING: Final = f"{KVM_DEVICE_PATH}:{KVM_DEVICE_PATH}:rw"
KVM_DEVICE_MAJOR: Final = 10
KVM_DEVICE_MINOR: Final = 232
KVM_RESOURCE_KEY: Final = "kvm"
_LOCAL_DOCKER_ADAPTER_URL: Final = "http+docker://localhost"
_LOCAL_UNIX_URL_PREFIXES: Final = ("unix://", "http+unix://")


def _invalid_kvm_request(message: str) -> HTTPException:
    return HTTPException(
        status_code=status.HTTP_400_BAD_REQUEST,
        detail={
            "code": SandboxErrorCodes.INVALID_PARAMETER,
            "message": message,
        },
    )


def resolve_kvm_device_group(
    resource_limits: Mapping[str, str],
    docker_base_url: str,
) -> int | None:
    """Validate an explicit local-Docker KVM request and return the device GID."""
    requested_value = resource_limits.get(KVM_RESOURCE_KEY)
    if requested_value is None:
        return None
    if requested_value != "1":
        raise _invalid_kvm_request("resourceLimits.kvm must be exactly '1'.")
    local_docker_endpoint = (
        docker_base_url.rstrip("/") == _LOCAL_DOCKER_ADAPTER_URL
        or docker_base_url.startswith(_LOCAL_UNIX_URL_PREFIXES)
    )
    if not local_docker_endpoint:
        raise _invalid_kvm_request(
            "resourceLimits.kvm is supported only with a local Unix Docker endpoint."
        )

    try:
        device = os.lstat(KVM_DEVICE_PATH)
    except OSError as exc:
        raise _invalid_kvm_request(
            f"resourceLimits.kvm requires {KVM_DEVICE_PATH} on the Docker host."
        ) from exc

    if not stat.S_ISCHR(device.st_mode):
        raise _invalid_kvm_request(
            f"resourceLimits.kvm requires {KVM_DEVICE_PATH} to be a character device."
        )
    if (
        os.major(device.st_rdev) != KVM_DEVICE_MAJOR
        or os.minor(device.st_rdev) != KVM_DEVICE_MINOR
    ):
        raise _invalid_kvm_request(
            f"resourceLimits.kvm requires {KVM_DEVICE_PATH} to be character device 10:232."
        )
    return device.st_gid


def apply_kvm_host_config(
    host_config_kwargs: dict[str, Any],
    device_group: int,
) -> dict[str, Any]:
    """Add the validated KVM device and its non-root group to Docker HostConfig."""
    updated = dict(host_config_kwargs)
    devices = list(updated.get("devices") or [])
    if KVM_DEVICE_MAPPING not in devices:
        devices.append(KVM_DEVICE_MAPPING)
    updated["devices"] = devices

    if device_group != 0:
        group_add = list(updated.get("group_add") or [])
        if str(device_group) not in {str(group) for group in group_add}:
            group_add.append(str(device_group))
        updated["group_add"] = group_add
    return updated
