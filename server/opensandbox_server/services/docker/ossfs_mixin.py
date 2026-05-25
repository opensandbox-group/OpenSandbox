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

"""OSSFS-specific Docker runtime behaviors."""

from __future__ import annotations

import logging
import os
import posixpath
import re
import subprocess
import tempfile
from typing import Any, Optional
from uuid import uuid4

from fastapi import HTTPException, status

from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.helpers import normalize_external_endpoint_url

logger = logging.getLogger(__name__)


class OSSFSMixin:
    @staticmethod
    def _validate_bucket_name(bucket: str) -> None:
        """
        Validate OSS bucket name to prevent command injection.
        
        Bucket names must follow OSS naming rules: lowercase letters, numbers, hyphens.
        Length: 3-63 characters. Cannot start/end with hyphen.
        """
        if not bucket or not isinstance(bucket, str):
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": "OSSFS bucket name cannot be empty.",
                },
            )
        
        # OSS bucket naming: 3-63 chars, lowercase alphanumeric and hyphens only
        # Must start and end with lowercase letter or digit
        if not re.match(r'^[a-z0-9]([a-z0-9-]{1,61}[a-z0-9])?$', bucket):
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": (
                        f"Invalid bucket name '{bucket}'. Bucket names must be 3-63 characters, "
                        "contain only lowercase letters, numbers, and hyphens, "
                        "and start/end with a letter or number."
                    ),
                },
            )

    @staticmethod
    def _validate_ossfs_option(option: str) -> None:
        """
        Validate OSSFS option to prevent command injection.
        
        Options should not contain shell metacharacters or command separators.
        """
        # Check for dangerous characters that could be used for command injection
        dangerous_chars = [';', '&', '|', '`', '$', '(', ')', '<', '>', '\n', '\r']
        for char in dangerous_chars:
            if char in option:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_PARAMETER,
                        "message": (
                            f"Invalid OSSFS option: contains forbidden character '{char}'. "
                            "Options must not contain shell metacharacters."
                        ),
                    },
                )

    @staticmethod
    def _validate_mount_path(path: str) -> None:
        """
        Validate mount path to prevent command injection in unmount operations.
        
        Path must be absolute and not contain dangerous characters.
        """
        if not path or not isinstance(path, str):
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": "Mount path cannot be empty.",
                },
            )
        
        # Path must be absolute
        if not path.startswith('/'):
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": f"Mount path must be absolute: '{path}'",
                },
            )
        
        # Check for dangerous characters that could be used for command injection
        dangerous_chars = [';', '&', '|', '`', '$', '(', ')', '<', '>', '\n', '\r']
        for char in dangerous_chars:
            if char in path:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_PARAMETER,
                        "message": (
                            f"Invalid mount path: contains forbidden character '{char}'. "
                            "Paths must not contain shell metacharacters."
                        ),
                    },
                )

    @staticmethod
    def _validate_endpoint_url(endpoint_url: str) -> None:
        """
        Validate endpoint URL to prevent command injection.
        
        URL should not contain dangerous shell metacharacters.
        """
        if not endpoint_url or not isinstance(endpoint_url, str):
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": "Endpoint URL cannot be empty.",
                },
            )
        
        # Check for dangerous characters
        dangerous_chars = [';', '&', '|', '`', '$', '(', ')', '<', '>', '\n', '\r', ' ']
        for char in dangerous_chars:
            if char in endpoint_url:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_PARAMETER,
                        "message": (
                            f"Invalid endpoint URL: contains forbidden character '{char}'. "
                            "URLs must not contain shell metacharacters or spaces."
                        ),
                    },
                )

    @staticmethod
    def _normalize_ossfs_option(raw_option: str) -> str:
        option = str(raw_option).strip()
        if not option:
            return ""
        return option

    def _resolve_ossfs_paths(self, volume) -> tuple[str, str]:
        """
        Resolve OSSFS base mount path and bind path.

        For OSSFS, ``volume.subPath`` represents the bucket prefix.
        The backend mount path and bind path are identical:
        - path = ossfs_mount_root/<bucket>/<subPath?>
        """
        mount_root = (self.app_config.storage.ossfs_mount_root or "").strip()
        if not mount_root.startswith("/"):
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.INVALID_OSSFS_MOUNT_ROOT,
                    "message": (
                        "storage.ossfs_mount_root must be configured as an absolute path."
                    ),
                },
            )

        mount_root = posixpath.normpath(mount_root)
        bucket_root = posixpath.normpath(posixpath.join(mount_root, volume.ossfs.bucket))
        prefix = (volume.sub_path or "").lstrip("/")
        backend_path = posixpath.normpath(posixpath.join(bucket_root, prefix))

        bucket_prefix = bucket_root if bucket_root.endswith("/") else bucket_root + "/"
        if backend_path != bucket_root and not backend_path.startswith(bucket_prefix):
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_SUB_PATH,
                    "message": (
                        f"Volume '{volume.name}': resolved OSSFS prefix escapes bucket root."
                    ),
                },
            )

        return backend_path, backend_path

    def _build_ossfs_v1_command(
        self,
        volume,
        source: str,
        backend_path: str,
        endpoint_url: str,
        passwd_file: str,
    ) -> list[str]:
        # Validate inputs for security
        self._validate_bucket_name(volume.ossfs.bucket)
        self._validate_endpoint_url(endpoint_url)
        self._validate_mount_path(backend_path)
        
        cmd: list[str] = [
            "ossfs",
            source,
            backend_path,
            "-o",
            f"url={endpoint_url}",
            "-o",
            f"passwd_file={passwd_file}",
        ]
        if volume.ossfs.options:
            for raw_opt in volume.ossfs.options:
                opt = self._normalize_ossfs_option(raw_opt)
                if opt:
                    # Validate each option for dangerous characters
                    self._validate_ossfs_option(opt)
                    cmd.extend(["-o", opt])
        return cmd

    def _build_ossfs_v2_config_lines(
        self,
        volume,
        endpoint_url: str,
        prefix: str,
    ) -> list[str]:
        # Validate inputs for security
        self._validate_bucket_name(volume.ossfs.bucket)
        self._validate_endpoint_url(endpoint_url)
        
        conf_lines: list[str] = [
            f"--oss_endpoint={endpoint_url}",
            f"--oss_bucket={volume.ossfs.bucket}",
            f"--oss_access_key_id={volume.ossfs.access_key_id}",
            f"--oss_access_key_secret={volume.ossfs.access_key_secret}",
        ]
        if prefix:
            normalized_prefix = prefix if prefix.endswith("/") else f"{prefix}/"
            conf_lines.append(f"--oss_bucket_prefix={normalized_prefix}")
        if volume.ossfs.options:
            for raw_opt in volume.ossfs.options:
                opt = self._normalize_ossfs_option(raw_opt)
                if opt:
                    # Validate each option for dangerous characters
                    self._validate_ossfs_option(opt)
                    conf_lines.append(f"--{opt}")
        return conf_lines

    def _build_ossfs_v2_mount_command(self, backend_path: str, conf_file: str) -> list[str]:
        # Validate backend path for security
        self._validate_mount_path(backend_path)
        return ["ossfs2", "mount", backend_path, "-c", conf_file]

    @staticmethod
    def _write_ossfs_private_config_file(file_path: str, content: str) -> None:
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        fd = os.open(file_path, flags, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            # OSSFS requires credentials in a local config/passwd file; the file
            # is created mode 0600 with an unpredictable UUID name and removed
            # immediately after the mount command completes.
            # codeql[py/clear-text-storage-sensitive-data]
            f.write(content)

    @staticmethod
    def _run_ossfs_mount_command(cmd: list[str], volume_name: str) -> None:
        result = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        if result.returncode != 0:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.OSSFS_MOUNT_FAILED,
                    "message": (
                        f"Volume '{volume_name}': failed to mount OSSFS backend. "
                        f"stderr={result.stderr.strip() or 'unknown error'}"
                    ),
                },
            )

    def _mount_ossfs_backend_path(self, volume, backend_path: str) -> None:
        """Mount OSS bucket/path to backend_path with version-specific OSSFS arguments."""
        access_key_id = volume.ossfs.access_key_id
        access_key_secret = volume.ossfs.access_key_secret
        if not access_key_id or not access_key_secret:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_OSSFS_CREDENTIALS,
                    "message": (
                        "OSSFS inline credentials are required: "
                        "accessKeyId and accessKeySecret must be provided."
                    ),
                },
            )
        os.makedirs(backend_path, exist_ok=True)

        bucket = volume.ossfs.bucket
        prefix = (volume.sub_path or "").strip("/")
        source = f"{bucket}:/{prefix}" if prefix else bucket
        endpoint = volume.ossfs.endpoint
        endpoint_url = normalize_external_endpoint_url(endpoint)

        passwd_file: Optional[str] = None
        conf_file: Optional[str] = None
        version = volume.ossfs.version or "2.0"
        try:
            if version == "1.0":
                passwd_file = os.path.join(
                    tempfile.gettempdir(),
                    f"opensandbox-ossfs-inline-{uuid4().hex}",
                )
                self._write_ossfs_private_config_file(
                    passwd_file,
                    f"{bucket}:{access_key_id}:{access_key_secret}",
                )
                cmd = self._build_ossfs_v1_command(
                    volume=volume,
                    source=source,
                    backend_path=backend_path,
                    endpoint_url=endpoint_url,
                    passwd_file=passwd_file,
                )
            elif version == "2.0":
                conf_lines = self._build_ossfs_v2_config_lines(
                    volume=volume,
                    endpoint_url=endpoint_url,
                    prefix=prefix,
                )
                conf_file = os.path.join(
                    tempfile.gettempdir(),
                    f"opensandbox-ossfs2-{uuid4().hex}.conf",
                )
                self._write_ossfs_private_config_file(conf_file, "\n".join(conf_lines) + "\n")
                cmd = self._build_ossfs_v2_mount_command(backend_path, conf_file)
            else:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_OSSFS_VERSION,
                        "message": (
                            f"Volume '{volume.name}': unsupported OSSFS version '{version}'."
                        ),
                    },
                )
            self._run_ossfs_mount_command(cmd, volume.name)
        except OSError as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.OSSFS_MOUNT_FAILED,
                    "message": (
                        f"Volume '{volume.name}': failed to execute ossfs command: {exc}"
                    ),
                },
            ) from exc
        finally:
            if passwd_file:
                try:
                    os.remove(passwd_file)
                except OSError:
                    pass
            if conf_file:
                try:
                    os.remove(conf_file)
                except OSError:
                    pass

    def _ensure_ossfs_mounted(self, volume_or_mount_key) -> str:
        """Ensure OSSFS backend path is mounted and return mount key."""
        if isinstance(volume_or_mount_key, str):
            mount_key = volume_or_mount_key
            backend_path = volume_or_mount_key
            volume = None
        else:
            volume = volume_or_mount_key
            backend_path, _ = self._resolve_ossfs_paths(volume)
            mount_key = backend_path

        with self._ossfs_mount_lock:
            current = self._ossfs_mount_ref_counts.get(mount_key, 0)
            if current > 0:
                self._ossfs_mount_ref_counts[mount_key] = current + 1
                return mount_key

            if not os.path.ismount(backend_path):
                if volume is None:
                    raise HTTPException(
                        status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                        detail={
                            "code": SandboxErrorCodes.OSSFS_MOUNT_FAILED,
                            "message": (
                                f"Failed to mount OSSFS path '{mount_key}': "
                                "missing volume context."
                            ),
                        },
                    )
                self._mount_ossfs_backend_path(volume, backend_path)

            self._ossfs_mount_ref_counts[mount_key] = 1
            return mount_key

    def _release_ossfs_mount(self, mount_key: str) -> None:
        """Release one reference and unmount when ref count reaches zero."""
        # Validate mount path before using in unmount commands
        self._validate_mount_path(mount_key)
        
        with self._ossfs_mount_lock:
            current = self._ossfs_mount_ref_counts.get(mount_key, 0)
            if current <= 0:
                logger.warning(
                    "Skipping OSSFS unmount for untracked mount key '%s'.",
                    mount_key,
                )
                return
            if current == 1:
                self._ossfs_mount_ref_counts.pop(mount_key, None)
                should_unmount = True
            else:
                self._ossfs_mount_ref_counts[mount_key] = current - 1
                should_unmount = False

        if not should_unmount or not os.path.ismount(mount_key):
            return

        errors: list[str] = []
        for cmd in (["fusermount", "-u", mount_key], ["umount", mount_key]):
            result = subprocess.run(
                cmd,
                capture_output=True,
                text=True,
                timeout=20,
                check=False,
            )
            if result.returncode == 0:
                return
            errors.append(result.stderr.strip() or "unknown error")

        raise HTTPException(
            status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
            detail={
                "code": SandboxErrorCodes.OSSFS_UNMOUNT_FAILED,
                "message": f"Failed to unmount OSSFS path '{mount_key}': {'; '.join(errors)}",
            },
        )

    def _release_ossfs_mounts(self, mount_keys: list[str]) -> None:
        for key in mount_keys:
            try:
                self._release_ossfs_mount(key)
            except HTTPException as exc:
                logger.warning("Failed to release OSSFS mount %s: %s", key, exc.detail)

    def _prepare_ossfs_mounts(self, volumes: Optional[list]) -> list[str]:
        if not volumes:
            return []
        key_to_volume: dict[str, Any] = {}
        prepared_mount_keys: list[str] = []
        for volume in volumes:
            if volume.ossfs is not None:
                mount_key, _ = self._resolve_ossfs_paths(volume)
                if mount_key not in key_to_volume:
                    key_to_volume[mount_key] = volume
        try:
            for mount_key, volume in key_to_volume.items():
                self._ensure_ossfs_mounted(volume)
                prepared_mount_keys.append(mount_key)
            return list(key_to_volume.keys())
        except Exception:
            # Roll back mounts already prepared in this batch.
            self._release_ossfs_mounts(prepared_mount_keys)
            raise

    def _validate_ossfs_volume(self, volume) -> None:
        """
        Docker-specific validation for OSSFS backend.

        Ensures inline credentials and path semantics are valid.
        """
        if os.name == "nt":
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": (
                        "OSSFS backend on Docker runtime requires a Linux host with FUSE support. "
                        "Running OpenSandbox Server on Windows is not supported for OSSFS mounts."
                    ),
                },
            )

        if not volume.ossfs.access_key_id or not volume.ossfs.access_key_secret:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_OSSFS_CREDENTIALS,
                    "message": (
                        "OSSFS inline credentials are required: "
                        "accessKeyId and accessKeySecret must be provided."
                    ),
                },
            )

        self._resolve_ossfs_paths(volume)
