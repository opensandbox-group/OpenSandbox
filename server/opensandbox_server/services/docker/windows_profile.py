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
Windows-profile helpers specific to Docker sandbox provisioning.

Mixed into DockerSandboxService via DockerContainerOpsMixin.
"""

from __future__ import annotations

import io
import logging
import os
import tarfile
import time
from threading import Lock
from typing import Callable, Optional, TYPE_CHECKING
from uuid import uuid4

from docker.errors import DockerException
from fastapi import HTTPException, status

from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.windows_common import is_windows_platform

if TYPE_CHECKING:
    from opensandbox_server.api.schema import PlatformSpec

WINDOWS_REQUIRED_DEVICES = ("/dev/kvm", "/dev/net/tun")
WINDOWS_REQUIRED_CAP_ADD = ("NET_ADMIN", "NET_RAW")
WINDOWS_OEM_VOLUME_PREFIX = "opensandbox-win-oem"


def resolve_docker_platform(platform: Optional["PlatformSpec"]) -> Optional[str]:
    """Resolve Docker API ``platform`` argument for container create.

    For windows profile (dockur/windows), the image itself is linux-based and
    should not be forced to windows/* via Docker API platform pinning.
    """
    if platform is None or is_windows_platform(platform):
        return None
    return f"{platform.os}/{platform.arch}"


def normalize_bootstrap_command(
    bootstrap_command: list[str],
    requested_windows_platform: bool,
) -> list[str]:
    # For linux profile, normalize single-string command with spaces
    # so bootstrap can exec reliably.
    if requested_windows_platform:
        return bootstrap_command
    if len(bootstrap_command) != 1 or " " not in bootstrap_command[0]:
        return bootstrap_command

    import shlex

    return shlex.split(bootstrap_command[0])


def validate_windows_runtime_prerequisites() -> None:
    """Validate host device paths required by dockur/windows runtime profile."""
    missing = [device for device in WINDOWS_REQUIRED_DEVICES if not os.path.exists(device)]
    if not missing:
        return
    raise HTTPException(
        status_code=status.HTTP_400_BAD_REQUEST,
        detail={
            "code": SandboxErrorCodes.INVALID_PARAMETER,
            "message": (
                "Windows profile requires host devices to be present: "
                f"{', '.join(missing)}."
            ),
        },
    )


def apply_windows_runtime_host_config_defaults(
    host_config_kwargs: dict,
    sandbox_id: str,
) -> dict:
    """Apply runtime defaults required by Windows profile."""
    updated = dict(host_config_kwargs)

    default_binds = [f"{WINDOWS_OEM_VOLUME_PREFIX}-{sandbox_id}:/oem:rw"]
    existing_binds = list(updated.get("binds") or [])
    updated["binds"] = existing_binds + default_binds

    existing_devices = list(updated.get("devices") or [])
    existing_devices.extend(WINDOWS_REQUIRED_DEVICES)
    updated["devices"] = existing_devices

    required_caps = set(WINDOWS_REQUIRED_CAP_ADD)
    cap_drop = [cap for cap in (updated.get("cap_drop") or []) if cap not in required_caps]
    if cap_drop:
        updated["cap_drop"] = cap_drop
    else:
        updated.pop("cap_drop", None)

    cap_add = set(updated.get("cap_add") or [])
    cap_add.update(WINDOWS_REQUIRED_CAP_ADD)
    updated["cap_add"] = sorted(cap_add)

    return updated


def fetch_execd_install_bat(
    *,
    docker_client,
    execd_image: str,
    cache: dict[str, bytes],
    cache_lock: Lock,
    docker_operation: Callable[[str, Optional[str]], object],
    logger: logging.Logger,
) -> bytes:
    """Fetch install.bat from execd image and memoize in caller-provided cache."""
    cached = cache.get("install_bat")
    if cached is not None:
        return cached

    with cache_lock:
        cached = cache.get("install_bat")
        if cached is not None:
            return cached

        container = None
        try:
            with docker_operation("execd install.bat cache create container", "execd-cache"):
                container = docker_client.containers.create(
                    image=execd_image,
                    command=["tail", "-f", "/dev/null"],
                    name=f"sandbox-execd-installbat-{uuid4()}",
                )
            with docker_operation("execd install.bat cache start container", "execd-cache"):
                container.start()
            with docker_operation("execd install.bat cache read archive", "execd-cache"):
                stream, _ = container.get_archive("/install.bat")
                tar_blob = b"".join(stream)
            with tarfile.open(fileobj=io.BytesIO(tar_blob), mode="r:*") as tar:
                member = next(
                    (m for m in tar.getmembers() if m.isfile() and m.name.endswith("install.bat")),
                    None,
                )
                if member is None:
                    raise HTTPException(
                        status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                        detail={
                            "code": SandboxErrorCodes.EXECD_DISTRIBUTION_FAILED,
                            "message": "install.bat was not found in execd image archive.",
                        },
                    )
                extracted = tar.extractfile(member)
                if extracted is None:
                    raise HTTPException(
                        status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                        detail={
                            "code": SandboxErrorCodes.EXECD_DISTRIBUTION_FAILED,
                            "message": "Failed to extract install.bat from execd image archive.",
                        },
                    )
                data = extracted.read()
        except HTTPException:
            raise
        except DockerException as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.EXECD_DISTRIBUTION_FAILED,
                    "message": f"Failed to fetch install.bat from execd image: {str(exc)}",
                },
            ) from exc
        finally:
            if container is not None:
                try:
                    with docker_operation("execd install.bat cache cleanup container", "execd-cache"):
                        container.remove(force=True)
                except DockerException as cleanup_exc:
                    logger.warning(
                        "Failed to cleanup temporary execd install.bat container: %s", cleanup_exc
                    )

        cache["install_bat"] = data
        return data


def fetch_execd_windows_binary(
    *,
    docker_client,
    execd_image: str,
    cache: dict[str, bytes],
    cache_lock: Lock,
    docker_operation: Callable[[str, Optional[str]], object],
    logger: logging.Logger,
) -> bytes:
    """Fetch execd.exe from execd image and memoize in caller-provided cache."""
    cached = cache.get("windows_execd_bin")
    if cached is not None:
        return cached

    with cache_lock:
        cached = cache.get("windows_execd_bin")
        if cached is not None:
            return cached

        container = None
        try:
            with docker_operation("execd windows bin cache create container", "execd-cache"):
                container = docker_client.containers.create(
                    image=execd_image,
                    command=["tail", "-f", "/dev/null"],
                    name=f"sandbox-execd-winbin-{uuid4()}",
                )
            with docker_operation("execd windows bin cache start container", "execd-cache"):
                container.start()
            with docker_operation("execd windows bin cache read archive", "execd-cache"):
                stream, _ = container.get_archive("/execd.exe")
                tar_blob = b"".join(stream)
            with tarfile.open(fileobj=io.BytesIO(tar_blob), mode="r:*") as tar:
                member = next(
                    (m for m in tar.getmembers() if m.isfile() and m.name.endswith("execd.exe")),
                    None,
                )
                if member is None:
                    raise HTTPException(
                        status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                        detail={
                            "code": SandboxErrorCodes.EXECD_DISTRIBUTION_FAILED,
                            "message": "execd.exe was not found in execd image archive.",
                        },
                    )
                extracted = tar.extractfile(member)
                if extracted is None:
                    raise HTTPException(
                        status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                        detail={
                            "code": SandboxErrorCodes.EXECD_DISTRIBUTION_FAILED,
                            "message": "Failed to extract execd.exe from execd image archive.",
                        },
                    )
                data = extracted.read()
        except HTTPException:
            raise
        except DockerException as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.EXECD_DISTRIBUTION_FAILED,
                    "message": f"Failed to fetch execd.exe from execd image: {str(exc)}",
                },
            ) from exc
        finally:
            if container is not None:
                try:
                    with docker_operation(
                        "execd windows bin cache cleanup container", "execd-cache"
                    ):
                        container.remove(force=True)
                except DockerException as cleanup_exc:
                    logger.warning(
                        "Failed to cleanup temporary execd windows bin container: %s", cleanup_exc
                    )

        cache["windows_execd_bin"] = data
        return data


def install_windows_oem_scripts(
    *,
    container,
    sandbox_id: str,
    install_bat_bytes: bytes,
    execd_windows_bin_bytes: bytes,
    ensure_directory: Callable[[object, str, Optional[str]], None],
    docker_operation: Callable[[str, Optional[str]], object],
) -> None:
    """Install OEM script for dockur/windows:
    - C:\\OEM\\install.bat from execd image
    """
    ensure_directory(container, "/oem", sandbox_id)

    tar_stream = io.BytesIO()
    with tarfile.open(fileobj=tar_stream, mode="w") as tar:
        install_script = tarfile.TarInfo(name="oem/install.bat")
        install_script.mode = 0o644
        install_script.size = len(install_bat_bytes)
        install_script.mtime = int(time.time())
        tar.addfile(install_script, io.BytesIO(install_bat_bytes))

        execd_bin = tarfile.TarInfo(name="oem/execd.exe")
        execd_bin.mode = 0o644
        execd_bin.size = len(execd_windows_bin_bytes)
        execd_bin.mtime = int(time.time())
        tar.addfile(execd_bin, io.BytesIO(execd_windows_bin_bytes))
    tar_stream.seek(0)
    try:
        with docker_operation("install windows OEM scripts", sandbox_id):
            container.put_archive(path="/", data=tar_stream.getvalue())
    except DockerException as exc:
        raise HTTPException(
            status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
            detail={
                "code": SandboxErrorCodes.BOOTSTRAP_INSTALL_FAILED,
                "message": f"Failed to install windows OEM scripts: {str(exc)}",
            },
        ) from exc
