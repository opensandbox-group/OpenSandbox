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

from datetime import datetime, timezone

import pytest
from fastapi import HTTPException

from opensandbox_server.api.schema import CreateSandboxRequest, ResourceLimits
from opensandbox_server.repositories.snapshots.sqlite import SQLiteSnapshotRepository
from opensandbox_server.services.snapshot_models import (
    SnapshotRecord,
    SnapshotRestoreConfig,
    SnapshotState,
    SnapshotStatusRecord,
)
from opensandbox_server.services.snapshot_restore import (
    DEFAULT_SNAPSHOT_RESTORE_ENTRYPOINT,
    resolve_sandbox_image_from_request,
)


def test_snapshot_restore_resolves_effective_image(monkeypatch, tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    repo.create(
        SnapshotRecord(
            id="snap-001",
            source_sandbox_id="sbx-001",
            restore_config=SnapshotRestoreConfig(image="registry.example.com/snapshots/snap-001:latest"),
            status=SnapshotStatusRecord(
                state=SnapshotState.READY,
                last_transition_at=datetime.now(timezone.utc),
            ),
        )
    )
    monkeypatch.setattr(
        "opensandbox_server.services.snapshot_restore.create_snapshot_repository",
        lambda: repo,
    )

    request = CreateSandboxRequest(
        snapshotId="snap-001",
        resourceLimits=ResourceLimits(root={"cpu": "500m"}),
    )

    resolved = resolve_sandbox_image_from_request(request)
    assert resolved.image is not None
    assert resolved.image.uri == "registry.example.com/snapshots/snap-001:latest"
    assert resolved.snapshot_id == "snap-001"
    assert resolved.entrypoint == DEFAULT_SNAPSHOT_RESTORE_ENTRYPOINT


def test_snapshot_restore_preserves_explicit_entrypoint(monkeypatch, tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    repo.create(
        SnapshotRecord(
            id="snap-003",
            source_sandbox_id="sbx-001",
            restore_config=SnapshotRestoreConfig(image="registry.example.com/snapshots/snap-003:latest"),
            status=SnapshotStatusRecord(
                state=SnapshotState.READY,
                last_transition_at=datetime.now(timezone.utc),
            ),
        )
    )
    monkeypatch.setattr(
        "opensandbox_server.services.snapshot_restore.create_snapshot_repository",
        lambda: repo,
    )

    request = CreateSandboxRequest(
        snapshotId="snap-003",
        resourceLimits=ResourceLimits(root={"cpu": "500m"}),
        entrypoint=["python", "app.py"],
    )

    resolved = resolve_sandbox_image_from_request(request)
    assert resolved.image is not None
    assert resolved.image.uri == "registry.example.com/snapshots/snap-003:latest"
    assert resolved.snapshot_id == "snap-003"
    assert resolved.entrypoint == ["python", "app.py"]


def test_snapshot_restore_rejects_unready_snapshot(monkeypatch, tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    repo.create(
        SnapshotRecord(
            id="snap-002",
            source_sandbox_id="sbx-001",
            restore_config=SnapshotRestoreConfig(image="registry.example.com/snapshots/snap-002:latest"),
            status=SnapshotStatusRecord(
                state=SnapshotState.CREATING,
                last_transition_at=datetime.now(timezone.utc),
            ),
        )
    )
    monkeypatch.setattr(
        "opensandbox_server.services.snapshot_restore.create_snapshot_repository",
        lambda: repo,
    )

    request = CreateSandboxRequest(
        snapshotId="snap-002",
        resourceLimits=ResourceLimits(root={"cpu": "500m"}),
    )

    with pytest.raises(HTTPException) as exc_info:
        resolve_sandbox_image_from_request(request)
    assert exc_info.value.status_code == 409
