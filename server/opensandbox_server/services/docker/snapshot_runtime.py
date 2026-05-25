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
Docker-backed snapshot runtime.

This runtime performs ``docker commit`` inline and returns the final status to
the caller so the server can persist terminal snapshot state in the request
path.
"""

from __future__ import annotations

import logging
from typing import Optional

from docker.errors import APIError, DockerException, ImageNotFound
from fastapi import HTTPException, status
from requests.exceptions import ConnectTimeout, ReadTimeout

from opensandbox_server.services.constants import SANDBOX_ID_LABEL, SandboxErrorCodes
from opensandbox_server.services.snapshot_models import SnapshotState
from opensandbox_server.services.snapshot_runtime import SnapshotRuntimeStatus

logger = logging.getLogger(__name__)

SNAPSHOT_IMAGE_REPOSITORY = "opensandbox-snapshots"


def build_snapshot_image_ref(snapshot_id: str) -> str:
    return f"{SNAPSHOT_IMAGE_REPOSITORY}:{snapshot_id}"


class DockerSnapshotRuntime:
    def __init__(self, docker_client) -> None:
        self._docker_client = docker_client

    def supports_create_snapshot(self) -> bool:
        return True

    def create_snapshot_unsupported_message(self) -> str:
        return ""

    def create_snapshot(
        self,
        snapshot_id: str,
        sandbox_id: str,
    ) -> Optional[SnapshotRuntimeStatus]:
        return self._create_snapshot(snapshot_id, sandbox_id)

    def get_snapshot_status(self, snapshot_id: str) -> Optional[SnapshotRuntimeStatus]:
        return None

    def delete_snapshot(self, snapshot_id: str, image: Optional[str] = None) -> None:
        image_ref = image or build_snapshot_image_ref(snapshot_id)
        try:
            self._docker_client.images.remove(image=image_ref)
        except ImageNotFound:
            logger.info("Docker snapshot image %s already absent for snapshot %s", image_ref, snapshot_id)
            return
        except APIError as exc:
            if getattr(exc, "status_code", None) == status.HTTP_409_CONFLICT:
                logger.info("Docker snapshot image %s cannot be deleted due to conflict: %s", image_ref, exc)
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

    def inspect_snapshot(self, snapshot_id: str, image: Optional[str] = None) -> SnapshotRuntimeStatus:
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
                "Unexpected error inspecting Docker snapshot image %s for snapshot %s: %s",
                image_ref,
                snapshot_id,
                exc,
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
            container.commit(
                repository=SNAPSHOT_IMAGE_REPOSITORY,
                tag=snapshot_id,
            )
        except (ReadTimeout, ConnectTimeout, TimeoutError) as exc:
            logger.warning(
                "Timed out creating Docker snapshot %s from sandbox %s: %s",
                snapshot_id,
                sandbox_id,
                exc,
            )
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_runtime_timeout",
                message=self._format_timeout_message(exc),
            )
        except Exception as exc:  # noqa: BLE001
            logger.exception(
                "Failed to create Docker snapshot %s from sandbox %s: %s",
                snapshot_id,
                sandbox_id,
                exc,
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

    def _get_container_by_sandbox_id(self, sandbox_id: str):
        label_selector = f"{SANDBOX_ID_LABEL}={sandbox_id}"
        try:
            containers = self._docker_client.containers.list(
                all=True,
                filters={"label": label_selector},
            )
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
