# Copyright 2025 Alibaba Group Holding Ltd.
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
Container operations mixin for Docker sandboxes.

Provides image management, platform resolution, and container creation utilities.
Mixed into DockerSandboxService.
"""

from __future__ import annotations

import logging
from datetime import datetime
from typing import Any, Dict, Optional

from docker.errors import DockerException, ImageNotFound
from docker.types import DeviceRequest
from fastapi import HTTPException, status

from opensandbox_server.extensions import (
    apply_access_renew_extend_seconds_to_mapping,
)

from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    PlatformSpec,
)
from opensandbox_server.services.constants import (
    SANDBOX_EXPIRES_AT_LABEL,
    SANDBOX_ID_LABEL,
    SANDBOX_MANUAL_CLEANUP_LABEL,
    SANDBOX_PLATFORM_ARCH_LABEL,
    SANDBOX_PLATFORM_OS_LABEL,
    SANDBOX_SNAPSHOT_ID_LABEL,
    SandboxErrorCodes,
)
from opensandbox_server.services.helpers import (
    parse_gpu_request,
    parse_memory_limit,
    parse_nano_cpus,
)
from opensandbox_server.services.docker.runtime import (
    BOOTSTRAP_PATH,
)

from opensandbox_server.services.docker.port_allocator import (
    normalize_container_port_spec,
)
from opensandbox_server.services.windows_common import (
    is_windows_platform,
)
from opensandbox_server.services.docker.windows_profile import (
    fetch_execd_install_bat,
    fetch_execd_windows_binary,
    install_windows_oem_scripts,
    normalize_bootstrap_command,
    resolve_docker_platform,
)

logger = logging.getLogger(__name__)


class DockerContainerOpsMixin:
    """Mixin providing image management, platform resolution, and container creation."""

    def _normalize_platform_key(self, platform: Optional[PlatformSpec]) -> str:
        if platform is None:
            return "default"
        return f"{platform.os}/{platform.arch}"

    @staticmethod
    def _normalize_arch(arch: Optional[str]) -> Optional[str]:
        if not isinstance(arch, str):
            return None
        normalized = arch.strip().lower()
        arch_aliases = {
            "x86_64": "amd64",
            "x86-64": "amd64",
            "amd64": "amd64",
            "aarch64": "arm64",
            "arm64/v8": "arm64",
            "arm64v8": "arm64",
            "arm64": "arm64",
        }
        return arch_aliases.get(normalized, normalized)

    @staticmethod
    def _normalize_os(os_value: Optional[str]) -> Optional[str]:
        if not isinstance(os_value, str):
            return None
        normalized = os_value.strip().lower()
        os_aliases = {
            "linux": "linux",
        }
        return os_aliases.get(normalized, normalized)

    def _get_daemon_platform(self) -> Optional[PlatformSpec]:
        if self._daemon_platform is not None:
            return self._daemon_platform
        try:
            info = self.docker_client.info() or {}
        except DockerException as exc:
            logger.debug("Failed to inspect Docker daemon platform: %s", exc)
            return None
        os_value = info.get("OSType") or info.get("Os") or info.get("os")
        arch_value = info.get("Architecture") or info.get("architecture")
        if not isinstance(os_value, str) or not isinstance(arch_value, str):
            return None
        normalized_os = self._normalize_os(os_value)
        normalized_arch = self._normalize_arch(arch_value)
        if not normalized_os or not normalized_arch:
            return None
        self._daemon_platform = PlatformSpec(os=normalized_os, arch=normalized_arch)
        return self._daemon_platform


    @staticmethod
    def _platform_from_labels(labels: Dict[str, str]) -> Optional[PlatformSpec]:
        os_value = labels.get(SANDBOX_PLATFORM_OS_LABEL)
        arch_value = labels.get(SANDBOX_PLATFORM_ARCH_LABEL)
        if not isinstance(os_value, str) or not isinstance(arch_value, str):
            return None
        if not os_value or not arch_value:
            return None
        normalized_os = DockerContainerOpsMixin._normalize_os(os_value)
        normalized_arch = DockerContainerOpsMixin._normalize_arch(arch_value)
        if not normalized_os or not normalized_arch:
            return None
        return PlatformSpec(os=normalized_os, arch=normalized_arch)

    def _resolve_platform_for_container(
        self,
        container,
        labels: Dict[str, str],
        include_runtime_metadata: bool = False,
    ) -> Optional[PlatformSpec]:
        # API contract: platform is only echoed from explicit constraints.
        # Runtime image metadata is used only for internal runtime preparation.
        if include_runtime_metadata:
            image_attrs = getattr(container.image, "attrs", {}) or {}
            if isinstance(image_attrs, dict):
                os_value = image_attrs.get("Os") or image_attrs.get("os")
                arch_value = image_attrs.get("Architecture") or image_attrs.get("architecture")
            else:
                os_value = None
                arch_value = None
            if isinstance(os_value, str) and isinstance(arch_value, str) and os_value and arch_value:
                normalized_os = self._normalize_os(os_value)
                normalized_arch = self._normalize_arch(arch_value)
                if normalized_os and normalized_arch:
                    return PlatformSpec(os=normalized_os, arch=normalized_arch)
        return self._platform_from_labels(labels)


    def _update_container_labels(self, container, labels: Dict[str, str]) -> None:
        """
        Update container labels, falling back to raw API if docker-py lacks support.
        """
        try:
            container.update(labels=labels)
        except TypeError:
            # Older docker-py versions do not accept labels; call low-level API directly.
            url = self.docker_client.api._url(f"/containers/{container.id}/update")  # noqa: SLF001
            data = {"Labels": labels}
            self.docker_client.api._post_json(url, data=data)  # noqa: SLF001
        container.reload()


    def _pull_image(
        self,
        image_uri: str,
        auth_config: Optional[dict],
        sandbox_id: str,
        platform: Optional[PlatformSpec] = None,
    ) -> None:
        docker_platform = resolve_docker_platform(platform)
        try:
            with self._docker_operation(f"pull image {image_uri}", sandbox_id):
                pull_kwargs: dict[str, Any] = {"auth_config": auth_config}
                if docker_platform is not None:
                    pull_kwargs["platform"] = docker_platform
                self.docker_client.images.pull(image_uri, **pull_kwargs)
        except TypeError as exc:
            if docker_platform is not None:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_PARAMETER,
                        "message": (
                            "The configured Docker client/daemon does not support "
                            f"platform-aware image pull for '{docker_platform}'."
                        ),
                    },
                ) from exc
            raise
        except DockerException as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.IMAGE_PULL_FAILED,
                    "message": f"Failed to pull image {image_uri}: {str(exc)}",
                },
            ) from exc

    def _ensure_image_available(
        self,
        image_uri: str,
        auth_config: Optional[dict],
        sandbox_id: str,
        platform: Optional[PlatformSpec] = None,
    ) -> None:
        expected_platform = platform
        if expected_platform is not None and is_windows_platform(expected_platform):
            expected_platform = None
        if expected_platform is None:
            expected_platform = self._get_daemon_platform()
        try:
            with self._docker_operation(f"inspect image {image_uri}", sandbox_id):
                image = self.docker_client.images.get(image_uri)
                if expected_platform is None:
                    logger.debug(
                        "Sandbox %s using cached image %s without platform check (daemon platform unavailable)",
                        sandbox_id,
                        image_uri,
                    )
                    return
                image_attrs = getattr(image, "attrs", {}) or {}
                image_os = (image_attrs.get("Os") or image_attrs.get("os") or "").lower()
                image_arch = (
                    image_attrs.get("Architecture")
                    or image_attrs.get("architecture")
                    or ""
                ).lower()
                image_os = self._normalize_os(image_os) or image_os
                image_arch = self._normalize_arch(image_arch) or image_arch
                requested_os = self._normalize_os(expected_platform.os) or expected_platform.os.lower()
                requested_arch = (
                    self._normalize_arch(expected_platform.arch) or expected_platform.arch.lower()
                )
                if image_os != requested_os or image_arch != requested_arch:
                    logger.info(
                        "Sandbox %s cached image %s platform mismatch (cached=%s/%s, requested=%s/%s); repulling",
                        sandbox_id,
                        image_uri,
                        image_os or "unknown",
                        image_arch or "unknown",
                        requested_os,
                        requested_arch,
                    )
                    self._pull_image(
                        image_uri,
                        auth_config,
                        sandbox_id,
                        expected_platform,
                    )
                    return
                logger.debug("Sandbox %s using cached image %s", sandbox_id, image_uri)
        except ImageNotFound:
            self._pull_image(
                image_uri,
                auth_config,
                sandbox_id,
                platform,
            )
        except DockerException as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.IMAGE_PULL_FAILED,
                    "message": f"Failed to inspect image {image_uri}: {str(exc)}",
                },
            ) from exc


    def _build_labels_and_env(
        self,
        sandbox_id: str,
        request: CreateSandboxRequest,
        expires_at: Optional[datetime],
    ) -> tuple[dict[str, str], list[str]]:
        metadata = request.metadata or {}
        labels = {key: str(value) for key, value in metadata.items()}
        labels[SANDBOX_ID_LABEL] = sandbox_id
        if expires_at is None:
            labels[SANDBOX_MANUAL_CLEANUP_LABEL] = "true"
        else:
            labels[SANDBOX_EXPIRES_AT_LABEL] = expires_at.isoformat()
        if request.platform is not None:
            labels[SANDBOX_PLATFORM_OS_LABEL] = request.platform.os
            labels[SANDBOX_PLATFORM_ARCH_LABEL] = request.platform.arch
        if request.snapshot_id:
            labels[SANDBOX_SNAPSHOT_ID_LABEL] = request.snapshot_id

        apply_access_renew_extend_seconds_to_mapping(labels, request.extensions)

        env_dict = request.env or {}
        environment = []
        for key, value in env_dict.items():
            if value is None:
                continue
            environment.append(f"{key}={value}")
        return labels, environment

    def _resolve_image_auth(
        self, request: CreateSandboxRequest, sandbox_id: str
    ) -> tuple[str, Optional[dict]]:
        image_uri = request.image.uri
        auth_config = None
        if request.image.auth:
            auth_config = {
                "username": request.image.auth.username,
                "password": request.image.auth.password,
            }
        self._ensure_image_available(image_uri, auth_config, sandbox_id, request.platform)
        return image_uri, auth_config

    def _resolve_resource_limits(
        self, request: CreateSandboxRequest
    ) -> tuple[Optional[int], Optional[int], Optional[int]]:
        resource_limits = (request.resource_limits.root if request.resource_limits else None) or {}
        mem_limit = parse_memory_limit(resource_limits.get("memory"))
        nano_cpus = parse_nano_cpus(resource_limits.get("cpu"))
        gpu_count = parse_gpu_request(resource_limits.get("gpu"))
        return mem_limit, nano_cpus, gpu_count

    def _base_host_config_kwargs(
        self,
        mem_limit: Optional[int],
        nano_cpus: Optional[int],
        network_mode: str,
        gpu_count: Optional[int] = None,
    ) -> Dict[str, Any]:
        host_config_kwargs: Dict[str, Any] = {"network_mode": network_mode}
        security_opts: list[str] = []
        docker_cfg = self.app_config.docker
        if docker_cfg.no_new_privileges:
            security_opts.append("no-new-privileges:true")
        if docker_cfg.apparmor_profile:
            security_opts.append(f"apparmor={docker_cfg.apparmor_profile}")
        if docker_cfg.seccomp_profile:
            security_opts.append(f"seccomp={docker_cfg.seccomp_profile}")
        if security_opts:
            host_config_kwargs["security_opt"] = security_opts
        if docker_cfg.drop_capabilities:
            host_config_kwargs["cap_drop"] = docker_cfg.drop_capabilities
        if docker_cfg.pids_limit is not None:
            host_config_kwargs["pids_limit"] = docker_cfg.pids_limit
        if mem_limit:
            host_config_kwargs["mem_limit"] = mem_limit
        if nano_cpus:
            host_config_kwargs["nano_cpus"] = nano_cpus
        if gpu_count:
            # Honors host toolchains such as nvidia-container-toolkit. The Docker
            # Engine returns a clear error at container create time if the host
            # cannot satisfy the request, so failure is surfaced rather than silent.
            host_config_kwargs["device_requests"] = [
                DeviceRequest(count=gpu_count, capabilities=[["gpu"]])
            ]
        # Inject secure runtime into host_config
        if self.docker_runtime:
            logger.info(
                "Using Docker runtime '%s' for container creation",
                self.docker_runtime,
            )
            host_config_kwargs["runtime"] = self.docker_runtime
        return host_config_kwargs

    def _create_and_start_container(
        self,
        sandbox_id: str,
        image_uri: str,
        bootstrap_command: list[str],
        labels: dict[str, str],
        environment: list[str],
        host_config_kwargs: Dict[str, Any],
        exposed_ports: Optional[list[str]],
        platform: Optional[PlatformSpec],
    ):
        requested_windows_platform = is_windows_platform(platform)
        bootstrap_command = normalize_bootstrap_command(
            bootstrap_command,
            requested_windows_platform,
        )
        docker_platform = resolve_docker_platform(platform)

        host_config = self.docker_client.api.create_host_config(**host_config_kwargs)
        container = None
        container_id: Optional[str] = None
        try:
            with self._docker_operation("create sandbox container", sandbox_id):
                container_kwargs = {
                    "image": image_uri,
                    "command": bootstrap_command,
                    "ports": (
                        [normalize_container_port_spec(p) for p in exposed_ports]
                        if exposed_ports
                        else None
                    ),
                    "name": f"sandbox-{sandbox_id}",
                    "environment": environment,
                    "labels": labels,
                    "host_config": host_config,
                }
                if not requested_windows_platform:
                    container_kwargs["entrypoint"] = [BOOTSTRAP_PATH]
                if docker_platform is not None:
                    container_kwargs["platform"] = docker_platform

                response = self.docker_client.api.create_container(**container_kwargs)
            container_id = response.get("Id")
            if not container_id:
                raise HTTPException(
                    status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                    detail={
                        "code": SandboxErrorCodes.CONTAINER_START_FAILED,
                        "message": "Docker did not return a container ID.",
                    },
                )
            container = self.docker_client.containers.get(container_id)
            runtime_platform = self._resolve_platform_for_container(
                container,
                labels,
                include_runtime_metadata=True,
            )
            if requested_windows_platform:
                install_bat_bytes = fetch_execd_install_bat(
                    docker_client=self.docker_client,
                    execd_image=self.execd_image,
                    cache=self._windows_profile_cache,
                    cache_lock=self._execd_archive_lock,
                    docker_operation=self._docker_operation,
                    logger=logger,
                )
                execd_windows_bin_bytes = fetch_execd_windows_binary(
                    docker_client=self.docker_client,
                    execd_image=self.execd_image,
                    cache=self._windows_profile_cache,
                    cache_lock=self._execd_archive_lock,
                    docker_operation=self._docker_operation,
                    logger=logger,
                )
                install_windows_oem_scripts(
                    container=container,
                    sandbox_id=sandbox_id,
                    install_bat_bytes=install_bat_bytes,
                    execd_windows_bin_bytes=execd_windows_bin_bytes,
                    ensure_directory=self._ensure_directory,
                    docker_operation=self._docker_operation,
                )
                logger.info(
                    "sandbox=%s | skip linux bootstrap/runtime injection for windows profile",
                    sandbox_id,
                )
            else:
                self._prepare_sandbox_runtime(container, sandbox_id, runtime_platform)
            with self._docker_operation("start sandbox container", sandbox_id):
                container.start()
            return container
        except Exception as exc:
            if container is not None:
                try:
                    with self._docker_operation("cleanup sandbox container", sandbox_id):
                        container.remove(force=True)
                except DockerException as cleanup_exc:
                    logger.warning(
                        "Failed to cleanup container for sandbox %s: %s",
                        sandbox_id,
                        cleanup_exc,
                    )
            elif container_id:
                try:
                    with self._docker_operation("cleanup sandbox container (API)", sandbox_id):
                        self.docker_client.api.remove_container(container_id, force=True)
                except DockerException as cleanup_exc:
                    logger.warning(
                        "Failed to cleanup container for sandbox %s: %s",
                        sandbox_id,
                        cleanup_exc,
                    )

            if isinstance(exc, HTTPException):
                raise exc
            if isinstance(exc, TypeError) and docker_platform is not None:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_PARAMETER,
                        "message": (
                            "The configured Docker client/daemon does not support "
                            f"platform-aware container create for '{docker_platform}'."
                        ),
                    },
                ) from exc

            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.CONTAINER_START_FAILED,
                    "message": f"Failed to create or start container: {str(exc)}",
                },
            ) from exc
