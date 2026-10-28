# Copyright 2025 The OpenSandbox Authors
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
Docker-backed snapshot runtime.

This runtime performs ``docker commit`` inline and returns the final status to
the caller so the server can persist terminal snapshot state in the request
path.
"""

from __future__ import annotations

import logging
from typing import Optional

from docker.errors import APIError, DockerException, ImageNotFound, NotFound as DockerNotFound
from fastapi import HTTPException, status
from requests.exceptions import ConnectTimeout, ReadTimeout

from opensandbox_server.services.constants import SANDBOX_ID_LABEL, SandboxErrorCodes
from opensandbox_server.services.snapshot_models import SnapshotState
from opensandbox_server.services.snapshot_runtime import (
    SnapshotRuntimePreflightError,
    SnapshotRuntimeStatus,
    SnapshotRuntimeUnsupportedError,
)

logger = logging.getLogger(__name__)


SNAPSHOT_IMAGE_REPOSITORY = "opensandbox-snapshots"

# gVisor keeps rootfs writes in an internal filestore when its default
# ``--overlay2=root:self`` is active. ``docker commit`` then captures only
# that opaque file (``/.gvisor.filestore.*``), so snapshots look READY while
# restoring none of the sandbox's writes. Require ``--overlay2=none``.
_GVISOR_FILESTORE_PREFIX = "/.gvisor.filestore"


def _basename(path: str | None) -> str:
    if not path:
        return ""
    return path.rsplit("/", 1)[-1]


def _is_gvisor_runtime_path(path: str | None) -> bool:
    name = _basename(path)
    return name == "runsc" or name.startswith("runsc-")


def _runtime_args_list(runtime_cfg: dict | None) -> list[str]:
    if not isinstance(runtime_cfg, dict):
        return []
    for key in ("runtimeArgs", "args", "Args", "runtime_args"):
        raw = runtime_cfg.get(key)
        if isinstance(raw, list):
            return [str(item) for item in raw]
        if isinstance(raw, str) and raw.strip():
            return raw.split()
    return []


def _has_overlay2_none(runtime_args: list[str]) -> bool:
    """Return True when args explicitly disable gVisor's root:self overlay."""
    for index, arg in enumerate(runtime_args):
        if arg == "--overlay2=none" or arg.startswith("--overlay2=none,"):
            return True
        if arg == "--overlay2" and index + 1 < len(runtime_args):
            if runtime_args[index + 1] == "none" or runtime_args[index + 1].startswith("none,"):
                return True
    return False


def _diff_paths(diff_entries) -> list[str]:
    paths: list[str] = []
    for entry in diff_entries or []:
        if isinstance(entry, dict):
            path = entry.get("Path") or entry.get("path")
        else:
            path = entry
        if isinstance(path, str) and path:
            paths.append(path)
    return paths


def _is_gvisor_filestore_only_diff(diff_entries) -> bool:
    paths = _diff_paths(diff_entries)
    if not paths:
        return False
    return all(
        path == _GVISOR_FILESTORE_PREFIX
        or path.startswith(_GVISOR_FILESTORE_PREFIX + ".")
        or path.startswith(_GVISOR_FILESTORE_PREFIX + "/")
        for path in paths
    )


def build_snapshot_image_ref(snapshot_id: str) -> str:
    return f"{SNAPSHOT_IMAGE_REPOSITORY}:{snapshot_id}"


class DockerSnapshotRuntime:
    def __init__(self, docker_client) -> None:
        self._docker_client = docker_client

    def supports_create_snapshot(self) -> bool:
        return True

    def supports_synchronous_create(self) -> bool:
        return False

    def create_snapshot_unsupported_message(self) -> str:
        return ""

    def preflight_create_snapshot(
        self,
        sandbox_id: str,
        *,
        namespace: str | None = None,
    ) -> None:
        del namespace  # Docker snapshots are labeled by sandbox id only.
        try:
            container = self._get_container_by_sandbox_id(sandbox_id)
        except (RuntimeError, ReadTimeout, ConnectTimeout, TimeoutError) as exc:
            raise SnapshotRuntimePreflightError(str(exc)) from exc

        try:
            self._ensure_gvisor_snapshot_safe(container, sandbox_id=sandbox_id)
        except (SnapshotRuntimeUnsupportedError, SnapshotRuntimePreflightError):
            raise
        except Exception as exc:  # noqa: BLE001
            raise SnapshotRuntimePreflightError(
                f"Cannot verify snapshot runtime for sandbox {sandbox_id}."
            ) from exc

    def create_snapshot(
        self,
        snapshot_id: str,
        sandbox_id: str,
        *,
        namespace: str | None = None,
    ) -> Optional[SnapshotRuntimeStatus]:
        return self._create_snapshot(snapshot_id, sandbox_id)

    def get_snapshot_status(self, snapshot_id: str) -> Optional[SnapshotRuntimeStatus]:
        return None

    def delete_snapshot(
        self,
        snapshot_id: str,
        image: Optional[str] = None,
        *,
        namespace: str | None = None,
        source_sandbox_id: str | None = None,
    ) -> None:
        image_ref = image or build_snapshot_image_ref(snapshot_id)
        try:
            self._docker_client.images.remove(image=image_ref)
        except ImageNotFound:
            logger.info(f"Docker snapshot image {image_ref} already absent for snapshot {snapshot_id}")
            return
        except APIError as exc:
            if getattr(exc, "status_code", None) == status.HTTP_409_CONFLICT:
                logger.info(f"Docker snapshot image {image_ref} cannot be deleted due to conflict: {exc}")
                raise HTTPException(
                    status_code=status.HTTP_409_CONFLICT,
                    detail={
                        "code": "SNAPSHOT::DELETE_CONFLICT",
                        "message": "snapshot image cannot be deleted due to a conflict",
                    },
                ) from exc
            raise RuntimeError(
                f"{SandboxErrorCodes.IMAGE_REMOVE_ERROR}: "
                f"Failed to delete snapshot image {image_ref}: {exc}"
            ) from exc
        except DockerException as exc:
            raise RuntimeError(
                f"{SandboxErrorCodes.IMAGE_REMOVE_ERROR}: "
                f"Failed to delete snapshot image {image_ref}: {exc}"
            ) from exc

    def inspect_snapshot(
        self,
        snapshot_id: str,
        image: Optional[str] = None,
        *,
        namespace: str | None = None,
        source_sandbox_id: str | None = None,
    ) -> SnapshotRuntimeStatus:
        image_ref = image or build_snapshot_image_ref(snapshot_id)
        try:
            self._docker_client.images.get(image_ref)
        except ImageNotFound:
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_recovery_missing_image",
                message="Snapshot creation was interrupted and no snapshot image was found.",
            )
        except DockerException as exc:
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_recovery_inspect_failed",
                message=f"Failed to inspect snapshot image {image_ref}: {exc}",
            )
        except Exception as exc:  # noqa: BLE001
            logger.exception(
                f"Unexpected error inspecting Docker snapshot image "
                f"{image_ref} for snapshot {snapshot_id}: {exc}"
            )
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_recovery_inspect_failed",
                message=f"Failed to inspect snapshot image {image_ref}: {exc}",
            )

        return SnapshotRuntimeStatus(
            state=SnapshotState.READY,
            image=image_ref,
            reason="snapshot_recovery_ready",
            message="Recovered snapshot image after server restart.",
        )

    def _create_snapshot(
        self,
        snapshot_id: str,
        sandbox_id: str,
    ) -> SnapshotRuntimeStatus:
        image_ref = build_snapshot_image_ref(snapshot_id)

        try:
            container = self._get_container_by_sandbox_id(sandbox_id)
            self._ensure_gvisor_snapshot_safe(container, sandbox_id=sandbox_id)
            self._reject_gvisor_filestore_only_diff(container, sandbox_id=sandbox_id)
            container.commit(
                repository=SNAPSHOT_IMAGE_REPOSITORY,
                tag=snapshot_id,
            )
        except (ReadTimeout, ConnectTimeout, TimeoutError) as exc:
            logger.warning(
                f"Timed out creating Docker snapshot {snapshot_id} from sandbox {sandbox_id}: {exc}"
            )
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_runtime_timeout",
                message=self._format_timeout_message(exc),
            )
        except SnapshotRuntimeUnsupportedError as exc:
            logger.warning(
                f"Refusing Docker snapshot {snapshot_id} from sandbox {sandbox_id}: {exc}"
            )
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_runtime_unsupported",
                message=str(exc),
            )
        except Exception as exc:  # noqa: BLE001
            logger.exception(
                f"Failed to create Docker snapshot {snapshot_id} from sandbox {sandbox_id}: {exc}"
            )
            reason = "snapshot_runtime_timeout" if self._is_timeout_error(exc) else "snapshot_runtime_failed"
            message = self._format_timeout_message(exc) if reason == "snapshot_runtime_timeout" else str(exc)
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason=reason,
                message=message,
            )

        return SnapshotRuntimeStatus(
            state=SnapshotState.READY,
            image=image_ref,
            reason="snapshot_runtime_ready",
            message="Docker snapshot image created successfully.",
        )


    def _ensure_gvisor_snapshot_safe(self, container, *, sandbox_id: str) -> None:
        """Reject gVisor runtimes that keep writes outside Docker's commit view."""
        runtime_name, runtime_cfg = self._resolve_container_runtime(container)
        if runtime_cfg is None and not runtime_name:
            return

        path = None
        if isinstance(runtime_cfg, dict):
            path = runtime_cfg.get("path") or runtime_cfg.get("Path")
        if not _is_gvisor_runtime_path(path) and not _is_gvisor_runtime_path(runtime_name):
            return

        runtime_args = _runtime_args_list(runtime_cfg if isinstance(runtime_cfg, dict) else None)
        if _has_overlay2_none(runtime_args):
            return

        display = runtime_name or _basename(path) or "runsc"
        raise SnapshotRuntimeUnsupportedError(
            f"Docker runtime {display!r} is gVisor (runsc) without --overlay2=none; "
            f"docker commit would silently drop sandbox {sandbox_id} writes kept in "
            f"gVisor's root:self overlay. Configure the runtime with --overlay2=none "
            f"(see docs/guides/secure-container.md) before creating snapshots."
        )

    def _reject_gvisor_filestore_only_diff(self, container, *, sandbox_id: str) -> None:
        """Fail closed when the only visible diff is gVisor's opaque filestore.

        If the filesystem diff cannot be inspected at all, abort rather than
        proceeding to ``docker commit`` — the same fail-closed spirit as the
        gVisor overlay preflight. A denied/timed-out diff endpoint must not
        allow an incomplete snapshot to be published as READY.
        """
        try:
            if hasattr(container, "diff"):
                diff_entries = container.diff()
            else:
                api = getattr(self._docker_client, "api", None)
                if api is None or not hasattr(api, "diff"):
                    raise SnapshotRuntimeUnsupportedError(
                        f"Cannot inspect filesystem diff before snapshotting sandbox "
                        f"{sandbox_id}: Docker diff API is unavailable. Refusing to "
                        f"commit a potentially incomplete gVisor snapshot."
                    )
                container_id = getattr(container, "id", None) or getattr(container, "short_id", None)
                if not container_id:
                    raise SnapshotRuntimeUnsupportedError(
                        f"Cannot inspect filesystem diff before snapshotting sandbox "
                        f"{sandbox_id}: container id is missing. Refusing to commit a "
                        f"potentially incomplete gVisor snapshot."
                    )
                diff_entries = api.diff(container_id)
        except SnapshotRuntimeUnsupportedError:
            raise
        except Exception as exc:  # noqa: BLE001
            raise SnapshotRuntimeUnsupportedError(
                f"Cannot inspect filesystem diff before snapshotting sandbox "
                f"{sandbox_id}: {exc}. Refusing to commit a potentially incomplete "
                f"gVisor snapshot."
            ) from exc

        if not _is_gvisor_filestore_only_diff(diff_entries):
            return

        raise SnapshotRuntimeUnsupportedError(
            f"Sandbox {sandbox_id} filesystem diff only lists gVisor filestore paths "
            f"({_GVISOR_FILESTORE_PREFIX}*); docker commit would produce an empty-looking "
            f"snapshot. Reconfigure the container runtime with --overlay2=none."
        )

    def _resolve_container_runtime(self, container) -> tuple[str | None, dict | None]:
        attrs = getattr(container, "attrs", None) or {}
        host_config = attrs.get("HostConfig") or {}
        runtime_name = host_config.get("Runtime") or host_config.get("runtime")
        if isinstance(runtime_name, str):
            runtime_name = runtime_name.strip() or None
        else:
            runtime_name = None

        try:
            info = self._docker_client.info()
        except Exception as exc:  # noqa: BLE001
            raise SnapshotRuntimePreflightError(
                f"Cannot read Docker daemon info to resolve container runtime: {exc}"
            ) from exc

        if not isinstance(info, dict):
            info = {}

        if not runtime_name:
            default_runtime = info.get("DefaultRuntime") or info.get("defaultRuntime")
            if isinstance(default_runtime, str) and default_runtime.strip():
                runtime_name = default_runtime.strip()

        runtimes = info.get("Runtimes") or {}
        runtime_cfg = None
        if runtime_name and isinstance(runtimes, dict):
            cfg = runtimes.get(runtime_name)
            if isinstance(cfg, dict):
                runtime_cfg = cfg

        return runtime_name, runtime_cfg

    def _get_container_by_sandbox_id(self, sandbox_id: str):
        label_selector = f"{SANDBOX_ID_LABEL}={sandbox_id}"
        try:
            containers = self._docker_client.containers.list(
                all=True,
                filters={"label": label_selector},
            )
        except DockerNotFound as exc:
            # Container disappeared between the list summary and the
            # follow-up inspect (docker-py issues one inspect per matched
            # container). Treat this as sandbox not found.
            raise RuntimeError(
                f"{SandboxErrorCodes.SANDBOX_NOT_FOUND}: Sandbox {sandbox_id} not found."
            ) from exc
        except DockerException as exc:
            raise RuntimeError(
                f"{SandboxErrorCodes.CONTAINER_QUERY_FAILED}: "
                f"Failed to query sandbox containers: {exc}"
            ) from exc

        if not containers:
            raise RuntimeError(
                f"{SandboxErrorCodes.SANDBOX_NOT_FOUND}: Sandbox {sandbox_id} not found."
            )

        return containers[0]

    def _format_timeout_message(self, exc: Exception) -> str:
        timeout_seconds = getattr(getattr(self._docker_client, "api", None), "timeout", None)
        if timeout_seconds is None:
            return f"Docker snapshot creation timed out: {exc}"
        return f"Docker snapshot creation timed out after {timeout_seconds} seconds: {exc}"

    @staticmethod
    def _is_timeout_error(exc: Exception) -> bool:
        return "timed out" in str(exc).lower()


__all__ = [
    "DockerSnapshotRuntime",
    "SNAPSHOT_IMAGE_REPOSITORY",
    "build_snapshot_image_ref",
]
