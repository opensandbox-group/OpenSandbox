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

from __future__ import annotations

from types import SimpleNamespace
from unittest.mock import MagicMock
import pytest

from opensandbox_server.services.docker.snapshot_runtime import (
    DockerSnapshotRuntime,
    build_snapshot_image_ref,
)
from opensandbox_server.services.snapshot_models import SnapshotState
from opensandbox_server.services.snapshot_runtime import (
    SnapshotRuntimeUnsupportedError,
)


def _container(*, runtime: str | None = None, diff=None, commit=None, diff_error=None):
    attrs = {"HostConfig": {}}
    if runtime is not None:
        attrs["HostConfig"]["Runtime"] = runtime

    def default_commit(*, repository: str, tag: str) -> None:
        return None

    container = SimpleNamespace(
        attrs=attrs,
        commit=commit or default_commit,
        id="ctr-1",
    )
    if diff_error is not None:
        def _raise_diff():
            raise diff_error

        container.diff = _raise_diff
    elif diff is not None:
        container.diff = lambda: diff
    else:
        # Default empty diff: safety check runs and allows ordinary runtimes.
        container.diff = lambda: []
    return container


def _docker_client(*, containers, runtimes=None, default_runtime=None):
    info = {"Runtimes": runtimes or {}}
    if default_runtime is not None:
        info["DefaultRuntime"] = default_runtime
    return SimpleNamespace(
        containers=SimpleNamespace(list=lambda **kwargs: containers),
        info=lambda: info,
        api=SimpleNamespace(timeout=60),
    )


def test_supports_synchronous_create_is_false_for_docker() -> None:
    runtime = DockerSnapshotRuntime(SimpleNamespace())

    assert runtime.supports_create_snapshot() is True
    assert runtime.supports_synchronous_create() is False


def test_create_snapshot_commits_container_and_marks_ready() -> None:
    commits: list[tuple[str, str]] = []

    def commit(*, repository: str, tag: str) -> None:
        commits.append((repository, tag))

    container = _container(runtime="runc", commit=commit)
    docker_client = _docker_client(
        containers=[container],
        runtimes={"runc": {"path": "runc"}},
    )

    runtime = DockerSnapshotRuntime(docker_client)
    status = runtime.create_snapshot("snap-001", "sbx-001")

    assert status is not None
    assert commits == [("opensandbox-snapshots", "snap-001")]
    assert status.state == SnapshotState.READY
    assert status.image == build_snapshot_image_ref("snap-001")
    assert runtime.get_snapshot_status("snap-001") is None


def test_create_snapshot_marks_failed_when_sandbox_container_missing() -> None:
    docker_client = _docker_client(containers=[])

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

    container = _container(runtime="runc", commit=commit)
    docker_client = _docker_client(
        containers=[container],
        runtimes={"runc": {"path": "runc"}},
    )
    docker_client.api = SimpleNamespace(timeout=timeout_seconds)

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


def test_preflight_rejects_gvisor_without_overlay2_none() -> None:
    container = _container(runtime="runsc")
    docker_client = _docker_client(
        containers=[container],
        runtimes={"runsc": {"path": "/usr/bin/runsc", "runtimeArgs": ["--platform=systrap"]}},
    )
    runtime = DockerSnapshotRuntime(docker_client)

    with pytest.raises(SnapshotRuntimeUnsupportedError, match="overlay2=none"):
        runtime.preflight_create_snapshot("sbx-001")


def test_preflight_allows_gvisor_with_overlay2_none() -> None:
    container = _container(runtime="runsc")
    docker_client = _docker_client(
        containers=[container],
        runtimes={
            "runsc": {
                "path": "/usr/bin/runsc",
                "runtimeArgs": ["--platform=systrap", "--overlay2=none"],
            }
        },
    )
    runtime = DockerSnapshotRuntime(docker_client)

    runtime.preflight_create_snapshot("sbx-001")


def test_preflight_allows_gvisor_with_split_overlay2_none() -> None:
    container = _container(runtime="custom-gvisor")
    docker_client = _docker_client(
        containers=[container],
        runtimes={
            "custom-gvisor": {
                "path": "/usr/local/bin/runsc",
                "runtimeArgs": ["--overlay2", "none"],
            }
        },
    )
    runtime = DockerSnapshotRuntime(docker_client)

    runtime.preflight_create_snapshot("sbx-001")


def test_preflight_allows_ordinary_runc_runtime() -> None:
    container = _container(runtime="runc")
    docker_client = _docker_client(
        containers=[container],
        runtimes={"runc": {"path": "runc"}},
    )
    runtime = DockerSnapshotRuntime(docker_client)

    runtime.preflight_create_snapshot("sbx-001")


def test_preflight_uses_default_runtime_when_hostconfig_runtime_empty() -> None:
    container = _container(runtime="")
    docker_client = _docker_client(
        containers=[container],
        default_runtime="runsc",
        runtimes={"runsc": {"path": "/usr/bin/runsc"}},
    )
    runtime = DockerSnapshotRuntime(docker_client)

    with pytest.raises(SnapshotRuntimeUnsupportedError, match="overlay2=none"):
        runtime.preflight_create_snapshot("sbx-001")


def test_create_snapshot_rejects_gvisor_filestore_only_diff() -> None:
    commits: list[tuple[str, str]] = []

    def commit(*, repository: str, tag: str) -> None:
        commits.append((repository, tag))

    container = _container(
        runtime="runsc",
        diff=[{"Path": "/.gvisor.filestore.123"}],
        commit=commit,
    )
    docker_client = _docker_client(
        containers=[container],
        runtimes={
            "runsc": {
                "path": "/usr/bin/runsc",
                "runtimeArgs": ["--overlay2=none"],
            }
        },
    )
    runtime = DockerSnapshotRuntime(docker_client)
    status = runtime.create_snapshot("snap-fs", "sbx-001")

    assert commits == []
    assert status is not None
    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_runtime_unsupported"
    assert "filestore" in (status.message or "")


def test_create_snapshot_still_commits_when_diff_has_real_paths() -> None:
    commits: list[tuple[str, str]] = []

    def commit(*, repository: str, tag: str) -> None:
        commits.append((repository, tag))

    container = _container(
        runtime="runc",
        diff=[{"Path": "/root/marker"}],
        commit=commit,
    )
    docker_client = _docker_client(
        containers=[container],
        runtimes={"runc": {"path": "runc"}},
    )
    runtime = DockerSnapshotRuntime(docker_client)
    status = runtime.create_snapshot("snap-ok", "sbx-001")

    assert commits == [("opensandbox-snapshots", "snap-ok")]
    assert status is not None
    assert status.state == SnapshotState.READY

def test_create_snapshot_aborts_when_diff_inspection_fails() -> None:
    commits: list[tuple[str, str]] = []

    def commit(*, repository: str, tag: str) -> None:
        commits.append((repository, tag))

    container = _container(
        runtime="runc",
        diff_error=RuntimeError("diff endpoint denied"),
        commit=commit,
    )
    docker_client = _docker_client(
        containers=[container],
        runtimes={"runc": {"path": "runc"}},
    )
    runtime = DockerSnapshotRuntime(docker_client)
    status = runtime.create_snapshot("snap-diff-fail", "sbx-001")

    assert commits == []
    assert status is not None
    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_runtime_unsupported"
    assert "filesystem diff" in (status.message or "")
    assert "diff endpoint denied" in (status.message or "")


def test_create_snapshot_aborts_when_diff_api_unavailable() -> None:
    commits: list[tuple[str, str]] = []

    def commit(*, repository: str, tag: str) -> None:
        commits.append((repository, tag))

    # No container.diff and no api.diff → safety check cannot run.
    container = SimpleNamespace(
        attrs={"HostConfig": {"Runtime": "runc"}},
        commit=commit,
        id="ctr-1",
    )
    docker_client = _docker_client(
        containers=[container],
        runtimes={"runc": {"path": "runc"}},
    )
    runtime = DockerSnapshotRuntime(docker_client)
    status = runtime.create_snapshot("snap-no-diff-api", "sbx-001")

    assert commits == []
    assert status is not None
    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_runtime_unsupported"
    assert "diff API is unavailable" in (status.message or "")

