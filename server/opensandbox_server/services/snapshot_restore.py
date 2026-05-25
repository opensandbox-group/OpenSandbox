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
Helpers for resolving sandbox create requests from snapshots.
"""

from __future__ import annotations

from fastapi import HTTPException, status

from opensandbox_server.api.schema import CreateSandboxRequest, ImageSpec
from opensandbox_server.repositories.snapshots.factory import create_snapshot_repository
from opensandbox_server.services.snapshot_models import SnapshotState

DEFAULT_SNAPSHOT_RESTORE_ENTRYPOINT = ["tail", "-f", "/dev/null"]


def resolve_sandbox_image_from_request(request: CreateSandboxRequest) -> CreateSandboxRequest:
    """
    Normalize a sandbox create request to an effective image-backed request.

    When `snapshotId` is used, this resolves the snapshot from server
    persistence and injects `request.image` from `restore_config.image`.
    """

    has_image = request.image is not None and bool(request.image.uri.strip())
    if has_image:
        return request

    snapshot_id = (request.snapshot_id or "").strip()
    if not snapshot_id:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": "SANDBOX::INVALID_PARAMETER",
                "message": "Either image or snapshotId must be provided.",
            },
        )

    snapshot_repository = create_snapshot_repository()
    snapshot = snapshot_repository.get(snapshot_id)
    if snapshot is None:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail={
                "code": "SNAPSHOT::NOT_FOUND",
                "message": f"Snapshot {snapshot_id} not found",
            },
        )

    if snapshot.status.state != SnapshotState.READY:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "code": "SNAPSHOT::NOT_READY",
                "message": f"Snapshot {snapshot_id} is not ready for restore.",
            },
        )

    restore_image = (snapshot.restore_config.image or "").strip()
    if not restore_image:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "code": "SNAPSHOT::INVALID_RESTORE_CONFIG",
                "message": f"Snapshot {snapshot_id} does not have a restorable image.",
            },
        )

    request.image = ImageSpec(uri=restore_image)
    request.snapshot_id = snapshot_id
    if not request.entrypoint:
        request.entrypoint = list(DEFAULT_SNAPSHOT_RESTORE_ENTRYPOINT)
    return request


__all__ = [
    "DEFAULT_SNAPSHOT_RESTORE_ENTRYPOINT",
    "resolve_sandbox_image_from_request",
]
