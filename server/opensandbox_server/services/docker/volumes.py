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
Volume management mixin for Docker sandboxes.

Provides volume validation, bind mount building, and volume cleanup.
Mixed into DockerSandboxService.
"""

from __future__ import annotations

import logging
import os
import posixpath
from typing import Optional

from docker.errors import DockerException, NotFound as DockerNotFound
from fastapi import HTTPException, status

from opensandbox_server.services.constants import (
    SANDBOX_MANAGED_VOLUMES_LABEL,
    SANDBOX_PLATFORM_OS_LABEL,
    SandboxErrorCodes,
)
from opensandbox_server.services.docker.windows_profile import WINDOWS_OEM_VOLUME_PREFIX
from opensandbox_server.services.validators import (
    ensure_valid_host_path,
    ensure_volumes_valid,
)

logger = logging.getLogger(__name__)


class DockerVolumesMixin:
    """Mixin providing volume validation, bind mount building, and cleanup."""

    def _validate_volumes(
        self, request
    ) -> tuple[dict[str, dict], list[str]]:
        """
        Validate volume definitions for Docker runtime.

        Performs comprehensive validation:
        - Calls shared volume validation (name, mount path, sub path, backend count)
        - Delegates to backend-specific validators for Docker-level checks

        Args:
            request: Sandbox creation request.

        Returns:
            A tuple of:
            - A dict mapping PVC volume names (``pvc.claimName``) to their
              ``docker volume inspect`` results.  Empty when there are no PVC
              volumes.  This data is passed to ``_build_volume_binds`` so that
              bind generation does not need a second API call.
            - A list of Docker named volume names that were auto-created during
              validation (empty when ``createIfNotExists`` is false or all
              volumes already existed).

        Raises:
            HTTPException: When any validation fails.
        """
        if not request.volumes:
            return {}, []

        # Shared validation: names, mount paths, sub paths, backend count, host path allowlist
        allowed_prefixes = self.app_config.storage.allowed_host_paths
        ensure_volumes_valid(request.volumes, allowed_host_prefixes=allowed_prefixes)

        pvc_inspect_cache: dict[str, dict] = {}
        auto_created_volumes: list[str] = []
        try:
            for volume in request.volumes:
                if volume.host is not None:
                    self._validate_host_volume(volume, allowed_prefixes)
                elif volume.pvc is not None:
                    vol_info, was_created = self._validate_pvc_volume(volume)
                    pvc_inspect_cache[volume.pvc.claim_name] = vol_info
                    if was_created and volume.pvc.delete_on_sandbox_termination:
                        auto_created_volumes.append(volume.pvc.claim_name)
                elif volume.ossfs is not None:
                    self._validate_ossfs_volume(volume)
        except Exception:
            # If any subsequent volume validation fails, remove volumes we
            # already auto-created so they don't leak — delete_sandbox will
            # never run for a sandbox that was never provisioned.
            self._cleanup_managed_volumes("<pre-sandbox>", auto_created_volumes)
            raise

        return pvc_inspect_cache, auto_created_volumes

    @staticmethod
    def _validate_host_volume(volume, allowed_prefixes: Optional[list[str]]) -> None:
        """
        Docker-specific validation for host bind mount volumes.

        Validates that the resolved host path (host.path + optional subPath)
        remains within allowed prefixes — including symlink resolution — then
        ensures the directory exists on the filesystem, creating it automatically
        if it does not.

        Args:
            volume: Volume with host backend.
            allowed_prefixes: Optional allowlist of host path prefixes.

        Raises:
            HTTPException: When the resolved path is invalid or cannot be created.
        """
        resolved_path = volume.host.path
        if volume.sub_path:
            resolved_path = os.path.normpath(os.path.join(resolved_path, volume.sub_path))

        # Defense in depth: re-validate the resolved path against the
        # allowlist.  Even though sub_path traversal (../) is blocked by
        # ensure_valid_sub_path(), normalizing and re-checking prevents
        # any edge-case bypass.
        if allowed_prefixes and resolved_path != volume.host.path:
            ensure_valid_host_path(resolved_path, allowed_prefixes)

        # ── Symlink-aware allowlist check ──
        # os.path.normpath and ensure_valid_host_path only perform lexical
        # checks.  A symlink within a whitelisted directory (e.g.
        # /data/opensandbox/link -> /) would pass lexical validation but
        # Docker resolves the symlink when creating the bind mount, escaping
        # the allowed prefix.  Resolve both the host path and the allowed
        # prefixes with realpath to detect this.
        if allowed_prefixes:
            canonical = os.path.realpath(resolved_path)
            canonical_prefixes = [os.path.realpath(p) for p in allowed_prefixes]
            if canonical != resolved_path or canonical_prefixes != allowed_prefixes:
                ensure_valid_host_path(canonical, canonical_prefixes)

        # Allow existing host files (for example ISO binds to /boot.iso)
        # without attempting directory creation.
        if os.path.isfile(resolved_path):
            return

        try:
            os.makedirs(resolved_path, exist_ok=True)
        except OSError as e:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.HOST_PATH_CREATE_FAILED,
                    "message": (
                        f"Volume '{volume.name}': could not ensure host path "
                        f"directory exists at '{resolved_path}': {type(e).__name__}"
                    ),
                },
            )

    def _validate_pvc_volume(self, volume) -> tuple[dict, bool]:
        """
        Docker-specific validation for PVC (named volume) backend.

        In Docker runtime, the ``pvc`` backend maps to a Docker named volume.
        ``pvc.claimName`` is used as the Docker volume name.  The volume must
        already exist (created via ``docker volume create``).

        When ``subPath`` is specified, the volume must use the ``local`` driver
        so that the host-side ``Mountpoint`` is a real filesystem path.  The
        resolved path (``Mountpoint + subPath``) is validated for path-traversal
        safety but *not* for existence, because the Mountpoint directory is
        typically owned by root and may not be stat-able by the server process.

        Args:
            volume: Volume with pvc backend.

        Returns:
            A tuple of:
            - The ``docker volume inspect`` result dict for the named volume.
            - Whether the volume was auto-created by this call.

        Raises:
            HTTPException: When the named volume does not exist, inspection
                fails, or subPath constraints are violated.
        """
        volume_name = volume.pvc.claim_name
        auto_created = False
        try:
            vol_info = self.docker_client.api.inspect_volume(volume_name)
        except DockerNotFound:
            if volume.pvc.create_if_not_exists:
                # Auto-create the Docker named volume
                try:
                    self.docker_client.api.create_volume(
                        name=volume_name,
                        labels={SANDBOX_MANAGED_VOLUMES_LABEL: "server"},
                    )
                    logger.info("Auto-created Docker named volume '%s'", volume_name)
                    vol_info = self.docker_client.api.inspect_volume(volume_name)
                    auto_created = True
                except DockerException as create_exc:
                    raise HTTPException(
                        status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                        detail={
                            "code": SandboxErrorCodes.PVC_VOLUME_INSPECT_FAILED,
                            "message": (
                                f"Volume '{volume.name}': failed to auto-create Docker "
                                f"named volume '{volume_name}': {create_exc}"
                            ),
                        },
                    ) from create_exc
            else:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.PVC_VOLUME_NOT_FOUND,
                        "message": (
                            f"Volume '{volume.name}': Docker named volume '{volume_name}' "
                            "does not exist. Named volumes must be created before sandbox "
                            "creation (e.g., 'docker volume create <name>')."
                        ),
                    },
                )
        except DockerException as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.PVC_VOLUME_INSPECT_FAILED,
                    "message": (
                        f"Volume '{volume.name}': failed to inspect Docker named volume "
                        f"'{volume_name}': {exc}"
                    ),
                },
            ) from exc

        # --- subPath validation for Docker named volumes ---
        if volume.sub_path:
            driver = vol_info.get("Driver", "")
            if driver != "local":
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.PVC_SUBPATH_UNSUPPORTED_DRIVER,
                        "message": (
                            f"Volume '{volume.name}': subPath is only supported for "
                            f"Docker named volumes using the 'local' driver, but "
                            f"volume '{volume_name}' uses driver '{driver}'."
                        ),
                    },
                )

            mountpoint = vol_info.get("Mountpoint", "")
            if not mountpoint:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.PVC_SUBPATH_UNSUPPORTED_DRIVER,
                        "message": (
                            f"Volume '{volume.name}': cannot resolve subPath because "
                            f"Docker named volume '{volume_name}' has no Mountpoint."
                        ),
                    },
                )

            resolved_path = posixpath.normpath(
                posixpath.join(mountpoint, volume.sub_path)
            )

            # ── Path-escape check (lexical + symlink) ──
            #
            # 1. Lexical check via normpath + path-boundary-aware startswith.
            #    Use mountpoint + "/" to avoid false positives when one
            #    mountpoint is a prefix of another (e.g., …/_data vs …/_data2).
            #    Docker Mountpoint paths are always POSIX, so use "/" directly.
            mountpoint_prefix = (
                mountpoint if mountpoint.endswith("/") else mountpoint + "/"
            )
            if resolved_path != mountpoint and not resolved_path.startswith(
                mountpoint_prefix
            ):
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_SUB_PATH,
                        "message": (
                            f"Volume '{volume.name}': resolved subPath escapes the "
                            f"volume mountpoint."
                        ),
                    },
                )

            # 2. Symlink-aware check (best-effort).
            #    Docker volume Mountpoint dirs are typically root-owned and not
            #    readable by the server process.  Using strict=True so that
            #    realpath raises OSError when it cannot traverse a directory
            #    instead of silently returning the unresolved lexical path
            #    (which would make this check a no-op).  When the path IS
            #    accessible, this detects symlink-escape attacks (e.g., a
            #    malicious symlink datasets -> /).
            try:
                canonical_mountpoint = os.path.realpath(
                    mountpoint, strict=True
                )
                canonical_resolved = os.path.realpath(
                    resolved_path, strict=True
                )
                # os.path.realpath returns OS-native separators, so use
                # os.sep here (unlike the lexical check above which operates
                # on POSIX-normalised Docker Mountpoint strings).
                canonical_prefix = (
                    canonical_mountpoint
                    if canonical_mountpoint.endswith(os.sep)
                    else canonical_mountpoint + os.sep
                )
                if (
                    canonical_resolved != canonical_mountpoint
                    and not canonical_resolved.startswith(canonical_prefix)
                ):
                    raise HTTPException(
                        status_code=status.HTTP_400_BAD_REQUEST,
                        detail={
                            "code": SandboxErrorCodes.INVALID_SUB_PATH,
                            "message": (
                                f"Volume '{volume.name}': resolved subPath escapes "
                                f"the volume mountpoint after symlink resolution."
                            ),
                        },
                    )
            except OSError:
                # Cannot access volume paths (expected for non-root server).
                # Lexical validation above is still enforced; the symlink
                # check is skipped because we cannot resolve the real paths.
                pass

            # NOTE: We intentionally do NOT check os.path.exists(resolved_path)
            # here.  Docker volume Mountpoint directories (e.g.,
            # /var/lib/docker/volumes/…/_data) are typically owned by root and
            # not readable by the server process.  os.path.exists() returns
            # False when the process lacks permission to stat the path, causing
            # false-negative rejections.  If the subPath does not actually
            # exist, Docker will report the error at container creation time.

        return vol_info, auto_created

    def _build_volume_binds(
        self,
        volumes: Optional[list],
        pvc_inspect_cache: Optional[dict[str, dict]] = None,
    ) -> list[str]:
        """
        Convert Volume definitions into Docker bind/volume mount specs.

        Supported backends:
        - ``host``: host path bind mount.
          Format: ``/host/path:/container/path:ro|rw``
        - ``pvc``: Docker named volume mount.
          Format (no subPath): ``volume-name:/container/path:ro|rw``
          Docker recognises non-absolute-path sources as named volume references.
          Format (with subPath): ``/var/lib/docker/volumes/…/subdir:/container/path:ro|rw``
          When subPath is specified, the volume's host Mountpoint (obtained from
          ``pvc_inspect_cache``) is used to produce a standard bind mount.
        - ``ossfs``: host bind mount to runtime-mounted OSSFS path.
          Format: ``/mnt/ossfs/<bucket>/<subPath?>:/container/path:ro|rw``

        Each mount string uses ``:ro`` for read-only and ``:rw`` for read-write
        (default).

        Args:
            volumes: List of Volume objects from the creation request.
            pvc_inspect_cache: Dict mapping PVC claimNames to their
                ``docker volume inspect`` results, populated by
                ``_validate_volumes``.  Avoids a redundant API call and
                eliminates the race window between validation and bind
                generation.

        Returns:
            List of Docker bind/volume mount strings.
        """
        if not volumes:
            return []

        cache = pvc_inspect_cache or {}
        binds: list[str] = []
        for volume in volumes:
            container_path = volume.mount_path
            mode = "ro" if volume.read_only else "rw"

            if volume.host is not None:
                # Resolve the concrete host path (host.path + optional subPath)
                host_path = volume.host.path
                if volume.sub_path:
                    host_path = os.path.normpath(
                        os.path.join(host_path, volume.sub_path)
                    )
                binds.append(f"{host_path}:{container_path}:{mode}")

            elif volume.pvc is not None:
                if volume.sub_path:
                    # Resolve the named volume's host-side Mountpoint and append
                    # the subPath to produce a regular bind mount.  Validation
                    # has already ensured the driver is "local" and the resolved
                    # path is safe.  Reuse cached inspect data to avoid a
                    # redundant Docker API call and potential race condition.
                    vol_info = cache.get(volume.pvc.claim_name, {})
                    mountpoint = vol_info.get("Mountpoint", "")
                    resolved = posixpath.normpath(
                        posixpath.join(mountpoint, volume.sub_path)
                    )
                    binds.append(f"{resolved}:{container_path}:{mode}")
                else:
                    # No subPath: use claimName directly as Docker volume ref.
                    binds.append(
                        f"{volume.pvc.claim_name}:{container_path}:{mode}"
                    )
            elif volume.ossfs is not None:
                _, host_path = self._resolve_ossfs_paths(volume)
                binds.append(f"{host_path}:{container_path}:{mode}")

        return binds

    def _cleanup_windows_oem_volume(
        self,
        sandbox_id: str,
        labels: Optional[dict[str, str]],
    ) -> None:
        """Best-effort cleanup for windows profile OEM named volume."""
        if labels is not None and labels.get(SANDBOX_PLATFORM_OS_LABEL) != "windows":
            return

        volume_name = f"{WINDOWS_OEM_VOLUME_PREFIX}-{sandbox_id}"
        try:
            with self._docker_operation("remove windows oem volume", sandbox_id):
                self.docker_client.api.remove_volume(volume_name)
        except DockerNotFound:
            return
        except DockerException as exc:
            logger.warning(
                "sandbox=%s | failed to remove windows OEM volume %s: %s",
                sandbox_id,
                volume_name,
                exc,
            )

    def _cleanup_managed_volumes(self, sandbox_id: str, volume_names: list[str]) -> None:
        """
        Remove Docker named volumes that were auto-created for this sandbox.

        Only volumes whose ``opensandbox.io/volume-managed-by`` label equals
        ``"server"`` are removed.  Pre-existing volumes are never touched.
        Errors are logged but do not propagate — volume cleanup is best-effort.
        """
        for name in volume_names:
            try:
                vol_info = self.docker_client.api.inspect_volume(name)
                vol_labels = vol_info.get("Labels") or {}
                if vol_labels.get(SANDBOX_MANAGED_VOLUMES_LABEL) != "server":
                    logger.debug(
                        "sandbox=%s | volume '%s' not managed by server, skipping removal",
                        sandbox_id, name,
                    )
                    continue
                self.docker_client.api.remove_volume(name)
                logger.info("sandbox=%s | removed managed volume '%s'", sandbox_id, name)
            except DockerNotFound:
                logger.debug(
                    "sandbox=%s | managed volume '%s' already removed", sandbox_id, name,
                )
            except DockerException as exc:
                logger.warning(
                    "sandbox=%s | failed to remove managed volume '%s': %s",
                    sandbox_id, name, exc,
                )
