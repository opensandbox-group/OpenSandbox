# Copyright 2026 The OpenSandbox Authors
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

"""Fork retries must not multiply resources or delete the source."""
import asyncio
from datetime import datetime, timezone
from types import SimpleNamespace
from unittest.mock import Mock

from fastapi import HTTPException
import pytest

from opensandbox_server.api.fork_schema import ForkSandboxRequest
from opensandbox_server.api.schema import Sandbox, SandboxStatus
from opensandbox_server.config import AppConfig
from opensandbox_server.repositories.forks import ForkRepository
from opensandbox_server.services.fork_config import export_docker
from opensandbox_server.services.fork_service import ForkService
from opensandbox_server.tenants.context import set_current_tenant
from opensandbox_server.tenants.models import TenantEntry


class Sandboxes:
    def __init__(self):
        self.targets = {}
        self.created = []
        self.deleted = []

    def get_sandbox(self, target_id):
        if target_id not in self.targets:
            raise HTTPException(404)
        return self.targets[target_id]

    async def create_sandbox(self, request):
        self.created.append(request)
        target = Sandbox(id=request._operation_sandbox_id, snapshotId=request.snapshot_id,
                         status=SandboxStatus(state="Running"), createdAt=datetime.now(timezone.utc))
        self.targets[target.id] = target
        return target

    def delete_sandbox(self, target_id):
        self.deleted.append(target_id)
        self.targets.pop(target_id, None)


class Snapshots:
    def __init__(self):
        self._snapshot_runtime = Mock()
        self.created = {}
        self.state = "Ready"
        self.deleted = []
        self.cleanup_error = False

    def create_snapshot(self, source_id, request, *, snapshot_id, rootfs_only, capture_namespace):
        assert rootfs_only
        return self.created.setdefault(snapshot_id, SimpleNamespace(id=snapshot_id))

    def get_snapshot(self, snapshot_id):
        return SimpleNamespace(id=snapshot_id, status=SimpleNamespace(state=self.state))

    def delete_snapshot(self, snapshot_id):
        if self.cleanup_error:
            raise HTTPException(409)
        self.deleted.append(snapshot_id)


@pytest.fixture
def setup(tmp_path, monkeypatch):
    config = AppConfig.model_validate({"runtime": {"type": "docker", "execd_image": "execd:test"}, "store": {"type": "sqlite", "path": str(tmp_path / "store.db")}})
    repository = ForkRepository(config)
    sandboxes, snapshots = Sandboxes(), Snapshots()
    service = ForkService(repository, sandboxes, snapshots)
    async def build(*args):
        return {"timeout": 1800, "resourceLimits": {"cpu": "1"}, "entrypoint": ["tail", "-f", "/dev/null"]}
    monkeypatch.setattr("opensandbox_server.services.fork_service.build_fork_config", build)
    set_current_tenant(None)
    yield service, repository, sandboxes, snapshots
    set_current_tenant(None)
    repository.close()


def submit(service, key="key"):
    return asyncio.run(service.fork("source", ForkSandboxRequest(timeout=1800), key))


def step(service, repository):
    record = repository.claim("worker")
    assert record is not None
    service.advance(record, "worker")
    return record


def release_lease(repository):
    with repository._connection() as conn:
        conn.execute("UPDATE sandbox_forks SET lease_until = 0")


def test_success_and_replay(setup):
    service, repo, sandboxes, snapshots = setup
    operation = submit(service)
    assert submit(service).id == operation.id
    step(service, repo)
    result = service.get(operation.id)
    assert result.status.state == "Succeeded"
    assert result.sandbox_id != "source"
    assert len(sandboxes.created) == len(snapshots.created) == 1
    assert not snapshots.deleted and not sandboxes.deleted
    assert repo.claim("other") is None


def test_idempotency_conflict(setup):
    service, *_ = setup
    submit(service)
    with pytest.raises(HTTPException) as caught:
        asyncio.run(service.fork("other", ForkSandboxRequest(timeout=1800), "key"))
    assert caught.value.status_code == 409


def test_lease_and_existing_target_recovery(setup):
    service, repo, sandboxes, snapshots = setup
    operation = submit(service)
    record = repo.claim("worker")
    assert repo.claim("other") is None
    target = Sandbox(id=record["target_id"], snapshotId=record["snapshot_id"],
                     status=SandboxStatus(state="Running"), createdAt=datetime.now(timezone.utc))
    sandboxes.targets[target.id] = target
    service.advance(record, "worker")
    assert service.get(operation.id).status.state == "Succeeded"
    assert not sandboxes.created


def test_snapshot_wait_recovery(setup):
    service, repo, sandboxes, snapshots = setup
    operation = submit(service)
    snapshots.state = "Creating"
    step(service, repo)
    assert service.get(operation.id).status.state == "Snapshotting"
    snapshots.state = "Ready"
    release_lease(repo)
    restored = ForkService(repo, sandboxes, snapshots)
    step(restored, repo)
    assert restored.get(operation.id).status.state == "Succeeded"
    assert len(snapshots.created) == 1


def test_failed_cleanup_retries(setup):
    service, repo, sandboxes, snapshots = setup
    operation = submit(service)
    snapshots.state = "Failed"
    snapshots.cleanup_error = True
    step(service, repo)
    assert service.get(operation.id).cleanup_pending
    snapshots.cleanup_error = False
    release_lease(repo)
    step(service, repo)
    assert not service.get(operation.id).cleanup_pending
    assert not sandboxes.deleted


def test_tenant_scope(setup):
    service, *_ = setup
    set_current_tenant(TenantEntry("a", "same"))
    operation = submit(service)
    set_current_tenant(TenantEntry("b", "same"))
    with pytest.raises(HTTPException) as caught:
        service.get(operation.id)
    assert caught.value.status_code == 404
    assert submit(service).id != operation.id


def test_null_or_batch_overrides_rejected():
    with pytest.raises(ValueError):
        ForkSandboxRequest(timeout=1800, overrides={"env": None})
    with pytest.raises(ValueError):
        ForkSandboxRequest(timeout=1800, count=3)


def test_docker_volume_rejected_before_export():
    service = Mock()
    service._get_container_by_sandbox_id.return_value.attrs = {
        "Mounts": [{"Destination": "/data"}], "Config": {}, "HostConfig": {},
    }
    service._resolve_platform_for_container.return_value = SimpleNamespace(os="linux")
    with pytest.raises(HTTPException) as caught:
        export_docker(service, "source")
    assert caught.value.detail["code"] == "FORK::VOLUMES_UNSUPPORTED"


def test_ambiguous_submission_never_creates_second_target(setup):
    service, repo, sandboxes, _ = setup
    operation = submit(service)
    record = repo.claim("worker")
    record["target_submitted"] = True
    repo.save(record, "worker")
    service.advance(record, "worker")
    assert not sandboxes.created
    assert service.get(operation.id).status.state == "Provisioning"


def test_expired_operation_cleans_only_its_target(setup):
    service, repo, sandboxes, _ = setup
    operation = submit(service)
    record = repo.claim("worker")
    record["deadline"] = 0
    target = Sandbox(id=record["target_id"], snapshotId=record["snapshot_id"],
                     status=SandboxStatus(state="Running"), createdAt=datetime.now(timezone.utc))
    sandboxes.targets[target.id] = target
    service.advance(record, "worker")
    result = service.get(operation.id)
    assert result.status.state == "Failed"
    assert not result.cleanup_pending
    assert sandboxes.deleted == [target.id]


def test_late_target_after_deadline_is_cleaned(setup):
    service, repo, sandboxes, snapshots = setup
    operation = submit(service)
    record = repo.claim("worker")
    record.update(deadline=0, target_submitted=True)
    service.advance(record, "worker")
    assert service.get(operation.id).cleanup_pending
    assert not snapshots.deleted
    target = Sandbox(id=record["target_id"], snapshotId=record["snapshot_id"],
                     status=SandboxStatus(state="Running"), createdAt=datetime.now(timezone.utc))
    sandboxes.targets[target.id] = target
    release_lease(repo)
    step(service, repo)
    assert not service.get(operation.id).cleanup_pending
    assert sandboxes.deleted == [target.id]
    assert snapshots.deleted == [record["snapshot_id"]]
    assert not sandboxes.created


def test_create_returning_pending_target_is_not_success(setup):
    service, repo, sandboxes, _ = setup
    operation = submit(service)
    create = sandboxes.create_sandbox
    async def pending(request):
        target = await create(request)
        target.status.state = "Pending"
        return target
    sandboxes.create_sandbox = pending
    step(service, repo)
    assert service.get(operation.id).status.state == "Provisioning"
    target = next(iter(sandboxes.targets.values()))
    target.status.state = "Running"
    release_lease(repo)
    step(service, repo)
    assert service.get(operation.id).status.state == "Succeeded"
    assert len(sandboxes.created) == 1


def test_snapshot_timeout_during_failed_cleanup_waits_for_late_artifact(setup):
    service, repo, _, snapshots = setup
    operation = submit(service)
    record = repo.claim("worker")
    record["deadline"] = 0
    snapshots.get_snapshot = lambda snapshot_id: SimpleNamespace(
        status=SimpleNamespace(state="Failed", reason="snapshot_runtime_timeout"))
    runtime = snapshots._snapshot_runtime
    runtime.inspect_snapshot.return_value = SimpleNamespace(state=SimpleNamespace(value="Creating"))
    service.advance(record, "worker")
    assert service.get(operation.id).cleanup_pending
    assert not snapshots.deleted
    runtime.inspect_snapshot.return_value = SimpleNamespace(state=SimpleNamespace(value="Ready"), image="fork:image")
    snapshots.cleanup_error = True
    release_lease(repo)
    step(service, repo)
    assert service.get(operation.id).cleanup_pending
    # Metadata deletion may fail after the late artifact was removed. Recovery
    # must not wait forever for that already deleted artifact to reappear.
    snapshots.cleanup_error = False
    runtime.inspect_snapshot.return_value = SimpleNamespace(state=SimpleNamespace(value="Failed"))
    release_lease(repo)
    step(service, repo)
    assert not service.get(operation.id).cleanup_pending
    runtime.delete_snapshot.assert_called_once()


def test_docker_fork_capture_neutralizes_source_image_config():
    from opensandbox_server.services.docker.snapshot_runtime import DockerSnapshotRuntime
    container = Mock()
    container.attrs = {"Config": {"Env": ["OPENSANDBOX_ID=source", "SECRET=value", "PATH=/custom"],
                                 "Labels": {"opensandbox.io/id": "source", "old": "tag"}}}
    docker = Mock()
    docker.containers.list.return_value = [container]
    result = DockerSnapshotRuntime(docker).create_rootfs_snapshot("snapshot", "source")
    assert result.state.value == "Ready"
    config = container.commit.call_args.kwargs["conf"]
    assert config["Env"] == ["OPENSANDBOX_ID=", "SECRET=",
                             "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"]
    assert config["Labels"] == {"opensandbox.io/id": "", "old": ""}
