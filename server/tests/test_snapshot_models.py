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

from datetime import datetime

from opensandbox_server.services.snapshot_models import (
    SnapshotRecord,
    SnapshotRestoreConfig,
    SnapshotState,
    SnapshotStatusRecord,
)


def test_snapshot_record_defaults_are_runtime_agnostic() -> None:
    record = SnapshotRecord(
        id="snap-001",
        source_sandbox_id="sbx-001",
        name="before-install",
        description="snapshot before dependency install",
    )

    assert record.id == "snap-001"
    assert record.source_sandbox_id == "sbx-001"
    assert record.restore_config.image is None
    assert record.status.state == SnapshotState.CREATING
    assert record.status.reason is None
    assert record.status.message is None
    assert isinstance(record.created_at, datetime)
    assert isinstance(record.updated_at, datetime)


def test_snapshot_record_supports_ready_restore_config() -> None:
    ready_at = datetime.utcnow()

    record = SnapshotRecord(
        id="snap-002",
        source_sandbox_id="sbx-002",
        restore_config=SnapshotRestoreConfig(
            image="registry.example.com/snapshots/snap-002:latest"
        ),
        status=SnapshotStatusRecord(
            state=SnapshotState.READY,
            reason="snapshot_ready",
            message="Snapshot is ready.",
            last_transition_at=ready_at,
        ),
    )

    assert record.restore_config.image == "registry.example.com/snapshots/snap-002:latest"
    assert record.status.state == SnapshotState.READY
    assert record.status.last_transition_at == ready_at
