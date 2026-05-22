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

import pytest
from pydantic import ValidationError

from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    CreateSnapshotRequest,
    Host,
    ImageSpec,
    ListSnapshotsRequest,
    OSSFS,
    PaginationInfo,
    PaginationRequest,
    PlatformSpec,
    PVC,
    ResourceLimits,
    Snapshot,
    SnapshotFilter,
    SnapshotStatus,
    Volume,
)



class TestHost:

    def test_valid_path(self):
        backend = Host(path="/data/opensandbox")
        assert backend.path == "/data/opensandbox"

    def test_valid_windows_path(self):
        backend = Host(path=r"D:\sandbox-mnt\ReMe")
        assert backend.path == r"D:\sandbox-mnt\ReMe"

    def test_path_required(self):
        with pytest.raises(ValidationError) as exc_info:
            Host()  # type: ignore
        errors = exc_info.value.errors()
        assert any(e["loc"] == ("path",) for e in errors)

    def test_serialization(self):
        backend = Host(path="/data/opensandbox")
        data = backend.model_dump()
        assert data == {"path": "/data/opensandbox"}

    def test_deserialization(self):
        data = {"path": "/data/opensandbox"}
        backend = Host.model_validate(data)
        assert backend.path == "/data/opensandbox"


class TestPVC:

    def test_valid_claim_name(self):
        backend = PVC(claim_name="my-pvc")
        assert backend.claim_name == "my-pvc"

    def test_claim_name_alias(self):
        data = {"claimName": "my-pvc"}
        backend = PVC.model_validate(data)
        assert backend.claim_name == "my-pvc"

    def test_serialization_uses_alias(self):
        backend = PVC(claim_name="my-pvc")
        data = backend.model_dump(by_alias=True, exclude_none=True)
        assert data == {
            "claimName": "my-pvc",
            "createIfNotExists": True,
            "deleteOnSandboxTermination": False,
        }

    def test_serialization_with_provisioning_hints(self):
        """Provisioning hints should serialize with aliases."""
        backend = PVC(
            claim_name="my-pvc",
            storage_class="ssd",
            storage="5Gi",
            access_modes=["ReadWriteOnce"],
        )
        data = backend.model_dump(by_alias=True, exclude_none=True)
        assert data == {
            "claimName": "my-pvc",
            "createIfNotExists": True,
            "deleteOnSandboxTermination": False,
            "storageClass": "ssd",
            "storage": "5Gi",
            "accessModes": ["ReadWriteOnce"],
        }

    def test_claim_name_required(self):
        with pytest.raises(ValidationError) as exc_info:
            PVC()  # type: ignore
        errors = exc_info.value.errors()
        assert any("claim_name" in str(e["loc"]) or "claimName" in str(e["loc"]) for e in errors)


class TestOSSFS:

    def test_valid_ossfs(self):
        backend = OSSFS(
            bucket="bucket-test-3",
            endpoint="oss-cn-hangzhou.aliyuncs.com",
            version="2.0",
            options=["allow_other"],
            access_key_id="AKIDEXAMPLE",
            access_key_secret="SECRETEXAMPLE",
        )
        assert backend.bucket == "bucket-test-3"
        assert backend.version == "2.0"
        assert backend.access_key_id == "AKIDEXAMPLE"

    def test_default_ossfs_version_is_2_0(self):
        backend = OSSFS(
            bucket="bucket-test-3",
            endpoint="oss-cn-hangzhou.aliyuncs.com",
            access_key_id="AKIDEXAMPLE",
            access_key_secret="SECRETEXAMPLE",
        )
        assert backend.version == "2.0"

    def test_inline_credentials_required(self):
        with pytest.raises(ValidationError):
            OSSFS(  # type: ignore
                bucket="bucket-test-3",
                endpoint="oss-cn-hangzhou.aliyuncs.com",
            )


class TestVolume:

    def test_valid_host_volume(self):
        volume = Volume(
            name="workdir",
            host=Host(path="/data/opensandbox"),
            mount_path="/mnt/work",
            read_only=False,
        )
        assert volume.name == "workdir"
        assert volume.host is not None
        assert volume.host.path == "/data/opensandbox"
        assert volume.mount_path == "/mnt/work"
        assert volume.read_only is False
        assert volume.pvc is None
        assert volume.sub_path is None

    def test_valid_pvc_volume(self):
        volume = Volume(
            name="models",
            pvc=PVC(claim_name="shared-models-pvc"),
            mount_path="/mnt/models",
            read_only=True,
        )
        assert volume.name == "models"
        assert volume.pvc is not None
        assert volume.pvc.claim_name == "shared-models-pvc"
        assert volume.mount_path == "/mnt/models"
        assert volume.read_only is True
        assert volume.host is None

    def test_valid_volume_with_subpath(self):
        volume = Volume(
            name="workdir",
            host=Host(path="/data/opensandbox"),
            mount_path="/mnt/work",
            read_only=False,
            sub_path="task-001",
        )
        assert volume.sub_path == "task-001"

    def test_valid_ossfs_volume(self):
        volume = Volume(
            name="data",
            ossfs=OSSFS(
                bucket="bucket-test-3",
                endpoint="oss-cn-hangzhou.aliyuncs.com",
                    access_key_id="AKIDEXAMPLE",
                access_key_secret="SECRETEXAMPLE",
            ),
            mount_path="/mnt/data",
            sub_path="task-001",
        )
        assert volume.ossfs is not None
        assert volume.ossfs.access_key_id == "AKIDEXAMPLE"
        assert volume.sub_path == "task-001"

    def test_no_backend_raises(self):
        with pytest.raises(ValidationError) as exc_info:
            Volume(
                name="workdir",
                mount_path="/mnt/work",
                read_only=False,
            )
        error_message = str(exc_info.value)
        assert "backend" in error_message.lower()

    def test_multiple_backends_raises(self):
        with pytest.raises(ValidationError) as exc_info:
            Volume(
                name="workdir",
                host=Host(path="/data/opensandbox"),
                pvc=PVC(claim_name="my-pvc"),
                mount_path="/mnt/work",
                read_only=False,
            )
        error_message = str(exc_info.value)
        assert "backend" in error_message.lower()

    def test_serialization_host_volume(self):
        volume = Volume(
            name="workdir",
            host=Host(path="/data/opensandbox"),
            mount_path="/mnt/work",
            read_only=False,
            sub_path="task-001",
        )
        data = volume.model_dump(by_alias=True, exclude_none=True)
        assert data == {
            "name": "workdir",
            "host": {"path": "/data/opensandbox"},
            "mountPath": "/mnt/work",
            "readOnly": False,
            "subPath": "task-001",
        }

    def test_serialization_pvc_volume(self):
        volume = Volume(
            name="models",
            pvc=PVC(claim_name="shared-models-pvc"),
            mount_path="/mnt/models",
            read_only=True,
        )
        data = volume.model_dump(by_alias=True, exclude_none=True)
        assert data == {
            "name": "models",
            "pvc": {
                "claimName": "shared-models-pvc",
                "createIfNotExists": True,
                "deleteOnSandboxTermination": False,
            },
            "mountPath": "/mnt/models",
            "readOnly": True,
        }

    def test_deserialization_host_volume(self):
        data = {
            "name": "workdir",
            "host": {"path": "/data/opensandbox"},
            "mountPath": "/mnt/work",
            "readOnly": False,
            "subPath": "task-001",
        }
        volume = Volume.model_validate(data)
        assert volume.name == "workdir"
        assert volume.host is not None
        assert volume.host.path == "/data/opensandbox"
        assert volume.mount_path == "/mnt/work"
        assert volume.read_only is False
        assert volume.sub_path == "task-001"


class TestSnapshots:

    def test_create_snapshot_request_accepts_optional_name(self):
        request = CreateSnapshotRequest(name="checkpoint-before-import")
        assert request.name == "checkpoint-before-import"

    def test_snapshot_serialization_uses_aliases(self):
        snapshot = Snapshot(
            id="snap-001",
            sandboxId="sbx-001",
            name="checkpoint-before-import",
            status=SnapshotStatus(state="Ready"),
            createdAt="2026-04-22T00:00:00Z",
        )
        data = snapshot.model_dump(by_alias=True, exclude_none=True)
        assert data["sandboxId"] == "sbx-001"
        assert data["createdAt"] == snapshot.created_at
        assert data["status"] == {"state": "Ready"}

    def test_list_snapshots_request_supports_alias_filter(self):
        request = ListSnapshotsRequest(
            filter=SnapshotFilter(sandboxId="sbx-001", state=["Ready"]),
            pagination=PaginationRequest(page=2, pageSize=50),
        )
        assert request.filter.sandbox_id == "sbx-001"
        assert request.pagination is not None
        assert request.pagination.page_size == 50

    def test_pagination_info_serializes_aliases(self):
        pagination = PaginationInfo(
            page=1,
            pageSize=20,
            totalItems=3,
            totalPages=1,
            hasNextPage=False,
        )
        data = pagination.model_dump(by_alias=True)
        assert data == {
            "page": 1,
            "pageSize": 20,
            "totalItems": 3,
            "totalPages": 1,
            "hasNextPage": False,
        }


class TestCreateSandboxRequestSnapshotCompat:

    def test_accepts_snapshot_id_without_entrypoint(self):
        request = CreateSandboxRequest(
            snapshotId="snap-001",
            resourceLimits=ResourceLimits(root={"cpu": "500m"}),
        )
        assert request.snapshot_id == "snap-001"
        assert request.image is None
        assert request.entrypoint is None

    def test_accepts_snapshot_id_with_entrypoint(self):
        request = CreateSandboxRequest(
            snapshotId="snap-001",
            resourceLimits=ResourceLimits(root={"cpu": "500m"}),
            entrypoint=["python", "app.py"],
        )
        assert request.snapshot_id == "snap-001"
        assert request.entrypoint == ["python", "app.py"]

    def test_treats_blank_image_uri_as_missing_image(self):
        request = CreateSandboxRequest(
            image=ImageSpec(uri="   "),
            snapshotId="snap-001",
            resourceLimits=ResourceLimits(root={"cpu": "500m"}),
        )
        assert request.image is None
        assert request.snapshot_id == "snap-001"

    def test_rejects_when_both_image_and_snapshot_missing(self):
        with pytest.raises(ValidationError):
            CreateSandboxRequest(
                resourceLimits=ResourceLimits(root={"cpu": "500m"}),
            )

    def test_deserialization_pvc_volume(self):
        data = {
            "name": "models",
            "pvc": {"claimName": "shared-models-pvc"},
            "mountPath": "/mnt/models",
            "readOnly": True,
        }
        volume = Volume.model_validate(data)
        assert volume.name == "models"
        assert volume.pvc is not None
        assert volume.pvc.claim_name == "shared-models-pvc"
        assert volume.mount_path == "/mnt/models"
        assert volume.read_only is True

    def test_serialization_ossfs_volume(self):
        volume = Volume(
            name="data",
            ossfs=OSSFS(
                bucket="bucket-test-3",
                endpoint="oss-cn-hangzhou.aliyuncs.com",
                    access_key_id="AKIDEXAMPLE",
                access_key_secret="SECRETEXAMPLE",
            ),
            mount_path="/mnt/data",
            read_only=False,
            sub_path="task-001",
        )
        data = volume.model_dump(by_alias=True, exclude_none=True)
        assert data["ossfs"]["bucket"] == "bucket-test-3"
        assert data["ossfs"]["accessKeyId"] == "AKIDEXAMPLE"
        assert data["subPath"] == "task-001"


class TestCreateSandboxRequestWithVolumes:

    def test_request_without_timeout_uses_manual_cleanup(self):
        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            resource_limits=ResourceLimits({"cpu": "500m", "memory": "512Mi"}),
            entrypoint=["python", "-c", "print('hello')"],
        )
        assert request.timeout is None

    def test_request_without_volumes(self):
        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=3600,
            resource_limits=ResourceLimits({"cpu": "500m", "memory": "512Mi"}),
            entrypoint=["python", "-c", "print('hello')"],
        )
        assert request.volumes is None
        assert request.secure_access is False

    def test_request_with_secure_access(self):
        request = CreateSandboxRequest.model_validate(
            {
                "image": {"uri": "python:3.11"},
                "timeout": 3600,
                "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
                "entrypoint": ["python", "-c", "print('hello')"],
                "secureAccess": True,
            }
        )
        assert request.secure_access is True

        data = request.model_dump(by_alias=True, exclude_none=True)
        assert data["secureAccess"] is True

    def test_request_with_empty_volumes(self):
        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=3600,
            resource_limits=ResourceLimits({"cpu": "500m", "memory": "512Mi"}),
            entrypoint=["python", "-c", "print('hello')"],
            volumes=[],
        )
        assert request.volumes == []

    def test_request_with_host_volume(self):
        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=3600,
            resource_limits=ResourceLimits({"cpu": "500m", "memory": "512Mi"}),
            entrypoint=["python", "-c", "print('hello')"],
            volumes=[
                Volume(
                    name="workdir",
                    host=Host(path="/data/opensandbox"),
                    mount_path="/mnt/work",
                    read_only=False,
                )
            ],
        )
        assert request.volumes is not None
        assert len(request.volumes) == 1
        assert request.volumes[0].name == "workdir"

    def test_request_with_pvc_volume(self):
        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=3600,
            resource_limits=ResourceLimits({"cpu": "500m", "memory": "512Mi"}),
            entrypoint=["python", "-c", "print('hello')"],
            volumes=[
                Volume(
                    name="models",
                    pvc=PVC(claim_name="shared-models-pvc"),
                    mount_path="/mnt/models",
                    read_only=True,
                )
            ],
        )
        assert request.volumes is not None
        assert len(request.volumes) == 1
        assert request.volumes[0].pvc is not None
        assert request.volumes[0].pvc.claim_name == "shared-models-pvc"

    def test_request_with_multiple_volumes(self):
        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=3600,
            resource_limits=ResourceLimits({"cpu": "500m", "memory": "512Mi"}),
            entrypoint=["python", "-c", "print('hello')"],
            volumes=[
                Volume(
                    name="workdir",
                    host=Host(path="/data/opensandbox"),
                    mount_path="/mnt/work",
                    read_only=False,
                ),
                Volume(
                    name="models",
                    pvc=PVC(claim_name="shared-models-pvc"),
                    mount_path="/mnt/models",
                    read_only=True,
                ),
            ],
        )
        assert request.volumes is not None
        assert len(request.volumes) == 2

    def test_request_with_platform(self):
        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=3600,
            platform=PlatformSpec(os="linux", arch="arm64"),
            resource_limits=ResourceLimits({"cpu": "500m", "memory": "512Mi"}),
            entrypoint=["python", "-c", "print('hello')"],
        )
        assert request.platform is not None
        assert request.platform.os == "linux"
        assert request.platform.arch == "arm64"

    def test_serialization_with_volumes(self):
        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=3600,
            resource_limits=ResourceLimits({"cpu": "500m", "memory": "512Mi"}),
            entrypoint=["python", "-c", "print('hello')"],
            volumes=[
                Volume(
                    name="workdir",
                    host=Host(path="/data/opensandbox"),
                    mount_path="/mnt/work",
                    read_only=False,
                    sub_path="task-001",
                )
            ],
        )
        data = request.model_dump(by_alias=True, exclude_none=True)
        assert "volumes" in data
        assert len(data["volumes"]) == 1
        assert data["volumes"][0]["name"] == "workdir"
        assert data["volumes"][0]["mountPath"] == "/mnt/work"
        assert data["volumes"][0]["readOnly"] is False
        assert data["volumes"][0]["subPath"] == "task-001"

    def test_deserialization_with_volumes(self):
        data = {
            "image": {"uri": "python:3.11"},
            "timeout": 3600,
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
            "entrypoint": ["python", "-c", "print('hello')"],
            "volumes": [
                {
                    "name": "workdir",
                    "host": {"path": "/data/opensandbox"},
                    "mountPath": "/mnt/work",
                    "readOnly": False,
                    "subPath": "task-001",
                },
                {
                    "name": "models",
                    "pvc": {"claimName": "shared-models-pvc"},
                    "mountPath": "/mnt/models",
                    "readOnly": True,
                },
            ],
        }
        request = CreateSandboxRequest.model_validate(data)
        assert request.volumes is not None
        assert len(request.volumes) == 2

        assert request.volumes[0].name == "workdir"
        assert request.volumes[0].host is not None
        assert request.volumes[0].host.path == "/data/opensandbox"
        assert request.volumes[0].mount_path == "/mnt/work"
        assert request.volumes[0].read_only is False
        assert request.volumes[0].sub_path == "task-001"

        assert request.volumes[1].name == "models"
        assert request.volumes[1].pvc is not None
        assert request.volumes[1].pvc.claim_name == "shared-models-pvc"
        assert request.volumes[1].mount_path == "/mnt/models"
        assert request.volumes[1].read_only is True

    def test_deserialization_with_platform(self):
        data = {
            "image": {"uri": "python:3.11"},
            "platform": {"os": "linux", "arch": "amd64"},
            "timeout": 3600,
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
            "entrypoint": ["python", "-c", "print('hello')"],
        }
        request = CreateSandboxRequest.model_validate(data)
        assert request.platform is not None
        assert request.platform.os == "linux"
        assert request.platform.arch == "amd64"

    def test_request_rejects_zero_timeout(self):
        with pytest.raises(ValidationError):
            CreateSandboxRequest(
                image=ImageSpec(uri="python:3.11"),
                timeout=0,
                resource_limits=ResourceLimits({"cpu": "500m"}),
                entrypoint=["python", "-c", "print('hello')"],
            )

    def test_request_allows_timeout_above_previous_hardcoded_limit(self):
        request = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            timeout=172800,
            resource_limits=ResourceLimits({"cpu": "500m", "memory": "512Mi"}),
            entrypoint=["python", "-c", "print('hello')"],
        )

        assert request.timeout == 172800


class TestCreateSandboxRequestPoolMode:
    """Tests for pool mode (extensions.poolRef) validation."""

    def test_pool_mode_accepts_only_pool_ref(self):
        """Happy path: poolRef only, no image/entrypoint/resourceLimits required."""
        request = CreateSandboxRequest(
            extensions={"poolRef": "my-pool"},
        )
        assert request.image is None
        assert request.entrypoint is None
        assert request.resource_limits is None
        assert request.extensions["poolRef"] == "my-pool"

    def test_pool_mode_accepts_pool_ref_with_optional_fields(self):
        """poolRef with optional env/metadata/timeout should be valid."""
        request = CreateSandboxRequest(
            extensions={"poolRef": "my-pool"},
            env={"KEY": "value"},
            metadata={"team": "test"},
            timeout=600,
        )
        assert request.extensions["poolRef"] == "my-pool"
        assert request.env == {"KEY": "value"}

    def test_pool_mode_rejects_snapshot_id_with_pool_ref(self):
        """snapshotId and poolRef cannot be used together."""
        with pytest.raises(ValidationError) as exc_info:
            CreateSandboxRequest(
                snapshotId="snap-001",
                extensions={"poolRef": "my-pool"},
            )
        errors = exc_info.value.errors()
        assert any("snapshotId" in str(e) and "poolRef" in str(e) for e in errors)

    def test_resource_limits_required_without_pool_ref(self):
        """Without poolRef, resourceLimits is still required (image mode)."""
        with pytest.raises(ValidationError):
            CreateSandboxRequest(
                image=ImageSpec(uri="python:3.11"),
                entrypoint=["python"],
            )

    def test_pool_mode_normalizes_blank_snapshot_id(self):
        """Blank snapshotId (e.g. whitespace) should be normalized to None in pool mode."""
        req = CreateSandboxRequest(
            extensions={"poolRef": "my-pool"},
            snapshotId="   ",
        )
        assert req.snapshot_id is None

    def test_pool_mode_ignores_blank_pool_ref(self):
        """Blank poolRef should not trigger pool mode."""
        with pytest.raises(ValidationError):
            CreateSandboxRequest(
                extensions={"poolRef": "   "},
            )


