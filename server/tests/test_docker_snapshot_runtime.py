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

from __future__ import annotations

from types import SimpleNamespace
from unittest.mock import MagicMock
import pytest

from opensandbox_server.services.docker.snapshot_runtime import (
    DockerSnapshotRuntime,
    build_snapshot_image_ref,
)
from opensandbox_server.services.snapshot_models import SnapshotState


def test_create_snapshot_commits_container_and_marks_ready() -> None:
    container = SimpleNamespace()
    commits: list[tuple[str, str]] = []

    def commit(*, repository: str, tag: str) -> None:
        commits.append((repository, tag))

    container.commit = commit
    docker_client = SimpleNamespace(
        containers=SimpleNamespace(
            list=lambda **kwargs: [container],
        )
    )

    runtime = DockerSnapshotRuntime(docker_client)
    status = runtime.create_snapshot("snap-001", "sbx-001")

    assert status is not None
    assert commits == [("opensandbox-snapshots", "snap-001")]
    assert status.state == SnapshotState.READY
    assert status.image == build_snapshot_image_ref("snap-001")
    assert runtime.get_snapshot_status("snap-001") is None


def test_create_snapshot_marks_failed_when_sandbox_container_missing() -> None:
    docker_client = SimpleNamespace(
        containers=SimpleNamespace(
            list=lambda **kwargs: [],
        )
    )

    runtime = DockerSnapshotRuntime(docker_client)
    status = runtime.create_snapshot("snap-404", "sbx-missing")

    assert status is not None
    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_runtime_failed"
    assert "DOCKER::SANDBOX_NOT_FOUND" in (status.message or "")


def test_create_snapshot_marks_failed_on_timeout() -> None:
    timeout_seconds = 45

    def commit(*, repository: str, tag: str) -> None:
        raise TimeoutError("timed out waiting for docker commit")

    container = SimpleNamespace(commit=commit)
    docker_client = SimpleNamespace(
        containers=SimpleNamespace(list=lambda **kwargs: [container]),
        api=SimpleNamespace(timeout=timeout_seconds),
    )

    runtime = DockerSnapshotRuntime(docker_client)
    status = runtime.create_snapshot("snap-timeout", "sbx-001")

    assert status is not None
    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_runtime_timeout"
    assert str(timeout_seconds) in (status.message or "")


def test_delete_snapshot_removes_image() -> None:
    removed: list[str] = []
    docker_client = SimpleNamespace(
        images=SimpleNamespace(remove=lambda image: removed.append(image)),
    )

    runtime = DockerSnapshotRuntime(docker_client)
    runtime.delete_snapshot("snap-001", image="opensandbox-snapshots:snap-001")

    assert removed == ["opensandbox-snapshots:snap-001"]


def test_delete_snapshot_ignores_missing_image() -> None:
    from docker.errors import ImageNotFound

    def remove(*, image: str) -> None:
        raise ImageNotFound("missing")

    docker_client = SimpleNamespace(
        images=SimpleNamespace(remove=remove),
    )

    runtime = DockerSnapshotRuntime(docker_client)
    runtime.delete_snapshot("snap-001", image="opensandbox-snapshots:snap-001")


def test_delete_snapshot_raises_on_docker_error() -> None:
    from docker.errors import DockerException

    def remove(*, image: str) -> None:
        raise DockerException("daemon unavailable")

    docker_client = SimpleNamespace(
        images=SimpleNamespace(remove=remove),
    )

    runtime = DockerSnapshotRuntime(docker_client)

    with pytest.raises(RuntimeError, match="SNAPSHOT_IMAGE_REMOVE_FAILED"):
        runtime.delete_snapshot("snap-001", image="opensandbox-snapshots:snap-001")


def test_delete_snapshot_returns_generic_conflict_on_delete_conflict() -> None:
    from docker.errors import APIError
    from fastapi import HTTPException

    response = MagicMock()
    response.status_code = 409
    response.reason = "Conflict"
    response.text = "conflict: unable to delete image because it is being used by running container"

    def remove(*, image: str) -> None:
        raise APIError("image is being used by running container", response=response)

    docker_client = SimpleNamespace(
        images=SimpleNamespace(remove=remove),
    )

    runtime = DockerSnapshotRuntime(docker_client)

    with pytest.raises(HTTPException) as exc_info:
        runtime.delete_snapshot("snap-001", image="opensandbox-snapshots:snap-001")

    assert exc_info.value.status_code == 409
    assert exc_info.value.detail["code"] == "SNAPSHOT::DELETE_CONFLICT"
    assert exc_info.value.detail["message"] == "snapshot image cannot be deleted due to a conflict"


def test_inspect_snapshot_marks_ready_when_image_exists() -> None:
    inspected: list[str] = []
    docker_client = SimpleNamespace(
        images=SimpleNamespace(get=lambda image: inspected.append(image)),
    )

    runtime = DockerSnapshotRuntime(docker_client)
    status = runtime.inspect_snapshot("snap-001")

    assert inspected == ["opensandbox-snapshots:snap-001"]
    assert status.state == SnapshotState.READY
    assert status.image == "opensandbox-snapshots:snap-001"
    assert status.reason == "snapshot_recovery_ready"


def test_inspect_snapshot_marks_failed_when_image_missing() -> None:
    from docker.errors import ImageNotFound

    def get(image: str) -> None:
        raise ImageNotFound("missing")

    docker_client = SimpleNamespace(
        images=SimpleNamespace(get=get),
    )

    runtime = DockerSnapshotRuntime(docker_client)
    status = runtime.inspect_snapshot("snap-missing")

    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_recovery_missing_image"
