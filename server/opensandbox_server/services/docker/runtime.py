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
Runtime preparation mixin for Docker sandboxes.

Provides execd archive distribution and bootstrap launcher installation.
Mixed into DockerSandboxService.
"""

from __future__ import annotations

import io
import logging
import posixpath
import tarfile
import time
from typing import Optional
from uuid import uuid4

from docker.errors import DockerException
from fastapi import HTTPException, status

from opensandbox_server.api.schema import PlatformSpec
from opensandbox_server.services.constants import SandboxErrorCodes

logger = logging.getLogger(__name__)

OPENSANDBOX_DIR = "/opt/opensandbox"
# Use posixpath for container-internal paths so they always use forward slashes,
# even when the server runs on Windows.
EXECED_INSTALL_PATH = posixpath.join(OPENSANDBOX_DIR, "execd")
BOOTSTRAP_PATH = posixpath.join(OPENSANDBOX_DIR, "bootstrap.sh")


class DockerRuntimeMixin:
    """Mixin providing execd distribution and bootstrap launcher installation."""

    def _fetch_execd_archive(self, platform: Optional[PlatformSpec] = None) -> bytes:
        """Fetch (and memoize) the execd archive by effective target platform."""
        cache_key = self._normalize_platform_key(platform)
        if cache_key in self._execd_archive_cache:
            return self._execd_archive_cache[cache_key]

        with self._execd_archive_lock:
            # Double-check locking to ensure only one thread initializes the cache
            if cache_key in self._execd_archive_cache:
                return self._execd_archive_cache[cache_key]

            container = None
            docker_platform = None
            if platform is not None:
                docker_platform = f"{platform.os}/{platform.arch}"
            try:
                self._ensure_image_available(
                    self.execd_image,
                    auth_config=None,
                    sandbox_id=f"execd-cache:{cache_key}",
                    platform=platform,
                )

                with self._docker_operation("execd cache create container", "execd-cache"):
                    create_kwargs: dict[str, any] = {
                        "image": self.execd_image,
                        "command": ["tail", "-f", "/dev/null"],
                        "name": f"sandbox-execd-{uuid4()}",
                        "detach": True,
                        "auto_remove": False,
                    }
                    if docker_platform is not None:
                        create_kwargs["platform"] = docker_platform
                    container = self.docker_client.containers.create(**create_kwargs)
                with self._docker_operation("execd cache start container", "execd-cache"):
                    container.start()
                    container.reload()
                    logger.info("Created sandbox execd archive for container %s", container.id)
            except TypeError as exc:
                if docker_platform is not None:
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
                raise
            except DockerException as exc:
                raise HTTPException(
                    status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
                    detail={
                        "code": SandboxErrorCodes.EXECD_START_FAILED,
                        "message": f"Failed to start execd container: {str(exc)}",
                    },
                ) from exc

            try:
                with self._docker_operation("execd cache read archive", "execd-cache"):
                    stream, _ = container.get_archive("/execd")
                    data = b"".join(stream)
            except DockerException as exc:
                raise HTTPException(
                    status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                    detail={
                        "code": SandboxErrorCodes.EXECD_DISTRIBUTION_FAILED,
                        "message": f"Failed to read execd artifacts: {str(exc)}",
                    },
                ) from exc
            finally:
                if container:
                    try:
                        with self._docker_operation("execd cache cleanup container", "execd-cache"):
                            container.remove(force=True)
                    except DockerException as cleanup_exc:
                        logger.warning(
                            "Failed to cleanup temporary execd container: %s", cleanup_exc
                        )

            self._execd_archive_cache[cache_key] = data
            logger.info("Dumped execd archive to memory for platform key %s", cache_key)
            return data

    def _ensure_directory(self, container, path: str, sandbox_id: Optional[str] = None) -> None:
        """Create a directory within the target container if it does not exist."""
        if not path or path == "/":
            return
        normalized_path = path.rstrip("/")
        if not normalized_path:
            return
        tar_stream = io.BytesIO()
        with tarfile.open(fileobj=tar_stream, mode="w") as tar:
            dir_info = tarfile.TarInfo(name=normalized_path.lstrip("/"))
            dir_info.type = tarfile.DIRTYPE
            dir_info.mode = 0o755
            dir_info.mtime = int(time.time())
            tar.addfile(dir_info)
        tar_stream.seek(0)
        try:
            with self._docker_operation(f"ensure directory {normalized_path}", sandbox_id):
                container.put_archive(path="/", data=tar_stream.getvalue())
        except DockerException as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.EXECD_DISTRIBUTION_FAILED,
                    "message": f"Failed to create directory {path} in sandbox: {str(exc)}",
                },
            ) from exc

    def _copy_execd_to_container(
        self,
        container,
        sandbox_id: str,
        platform: Optional[PlatformSpec] = None,
    ) -> None:
        """Copy execd artifacts from the platform container into the sandbox."""
        archive = self._fetch_execd_archive(platform)
        target_parent = posixpath.dirname(EXECED_INSTALL_PATH.rstrip("/")) or "/"
        self._ensure_directory(container, target_parent, sandbox_id)
        try:
            with self._docker_operation("copy execd archive to sandbox", sandbox_id):
                container.put_archive(path=target_parent, data=archive)
        except DockerException as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.EXECD_DISTRIBUTION_FAILED,
                    "message": f"Failed to copy execd into sandbox: {str(exc)}",
                },
            ) from exc

    def _install_bootstrap_script(self, container, sandbox_id: str) -> None:
        """Install the bootstrap launcher that starts execd then chains to user command."""
        script_path = BOOTSTRAP_PATH
        script_dir = posixpath.dirname(script_path)
        self._ensure_directory(container, script_dir, sandbox_id)
        execd_binary = EXECED_INSTALL_PATH
        script_content = "\n".join(
            [
                "#!/bin/sh",
                "set -e",
                f"  {execd_binary} >/tmp/execd.log 2>&1 &",
                'exec "$@"',
                "",
            ]
        ).encode("utf-8")

        tar_stream = io.BytesIO()
        with tarfile.open(fileobj=tar_stream, mode="w") as tar:
            info = tarfile.TarInfo(name=script_path.lstrip("/"))
            info.mode = 0o755
            info.size = len(script_content)
            info.mtime = int(time.time())
            tar.addfile(info, io.BytesIO(script_content))
        tar_stream.seek(0)
        try:
            with self._docker_operation("install bootstrap script", sandbox_id):
                container.put_archive(path="/", data=tar_stream.getvalue())
        except DockerException as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.BOOTSTRAP_INSTALL_FAILED,
                    "message": f"Failed to install bootstrap launcher: {str(exc)}",
                },
            ) from exc

    def _prepare_sandbox_runtime(
        self,
        container,
        sandbox_id: str,
        platform: Optional[PlatformSpec] = None,
    ) -> None:
        """Copy execd artifacts and bootstrap launcher into the sandbox container."""
        self._copy_execd_to_container(container, sandbox_id, platform)
        self._install_bootstrap_script(container, sandbox_id)
