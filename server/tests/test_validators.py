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
from fastapi import HTTPException

from opensandbox_server.api.schema import Host, OSSFS, PVC, Volume, PlatformSpec
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.validators import (
    ensure_metadata_labels,
    ensure_platform_valid,
    ensure_timeout_within_limit,
    ensure_valid_host_path,
    ensure_valid_mount_path,
    ensure_valid_pvc_name,
    ensure_valid_sub_path,
    ensure_valid_volume_name,
    ensure_volumes_valid,
)

def test_ensure_platform_valid_accepts_windows_amd64():
    platform = PlatformSpec(os="windows", arch="amd64")
    assert ensure_platform_valid(platform) is None
    assert platform.os == "windows"
    assert platform.arch == "amd64"


def test_ensure_platform_valid_accepts_windows_arm64():
    platform = PlatformSpec(os="windows", arch="arm64")
    assert ensure_platform_valid(platform) is None
    assert platform.os == "windows"
    assert platform.arch == "arm64"

def test_ensure_platform_valid_rejects_unsupported_os():
    platform = PlatformSpec(os="darwin", arch="amd64")
    with pytest.raises(HTTPException) as exc_info:
        assert ensure_platform_valid(platform) is None
    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER

def test_ensure_metadata_labels_accepts_common_k8s_forms():
    valid_metadata = {
        "app": "web",
        "k8s.io/name": "app-1",
        "example.com/label": "a.b_c-1",
        "team": "A1_b-2.c",
        "empty": "",
    }

    assert ensure_metadata_labels(valid_metadata) is None

def test_ensure_metadata_labels_allows_none_or_empty():
    assert ensure_metadata_labels(None) is None
    assert ensure_metadata_labels({}) is None

def test_ensure_metadata_labels_rejects_name_too_long():
    """Label name part exceeding 63 characters should be rejected."""
    long_name = "a" * 64
    with pytest.raises(HTTPException) as exc_info:
        assert ensure_metadata_labels({long_name: "value"}) is None
    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL

def test_ensure_metadata_labels_rejects_prefix_too_long():
    """Label prefix (DNS subdomain) exceeding 253 characters should be rejected."""
    # Build a prefix that is longer than 253 chars: 5 labels of 62 chars = 314 chars
    label_part = "a" * 62
    long_prefix = ".".join([label_part] * 5)  # 62*5 + 4 = 314 chars
    key = f"{long_prefix}/name"
    with pytest.raises(HTTPException) as exc_info:
        assert ensure_metadata_labels({key: "value"}) is None
    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL

def test_ensure_metadata_labels_accepts_key_with_max_length_prefix_and_name():
    """Valid key where prefix <= 253 chars and name <= 63 chars but total > 253 should be accepted."""
    label_part = "a" * 62
    prefix = ".".join([label_part] * 4)
    assert len(prefix) == 251
    key = f"{prefix}/valid-name"
    assert ensure_metadata_labels({key: "value"}) is None

def test_ensure_metadata_labels_rejects_invalid_prefix_format():
    """Label prefix with invalid DNS subdomain characters should be rejected."""
    with pytest.raises(HTTPException) as exc_info:
        assert ensure_metadata_labels({"INVALID_PREFIX.io/name": "value"}) is None
    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL

def test_ensure_metadata_labels_rejects_value_too_long():
    """Label value exceeding 63 characters should be rejected."""
    long_value = "a" * 64
    with pytest.raises(HTTPException) as exc_info:
        assert ensure_metadata_labels({"app": long_value}) is None
    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL

def test_ensure_metadata_labels_rejects_key_with_empty_prefix():
    """Key with an empty prefix (starts with '/') should be rejected."""
    with pytest.raises(HTTPException) as exc_info:
        assert ensure_metadata_labels({"/name": "value"}) is None
    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL

def test_ensure_metadata_labels_rejects_reserved_prefix():
    """User metadata must not use the opensandbox.io/ reserved prefix."""
    with pytest.raises(HTTPException) as exc_info:
        assert ensure_metadata_labels({"opensandbox.io/expires-at": "2030-01-01T00:00:00Z"}) is None
    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL
    assert "reserved prefix" in exc_info.value.detail["message"]

def test_ensure_metadata_labels_rejects_manual_cleanup_key():
    """User must not inject the manual-cleanup lifecycle label."""
    with pytest.raises(HTTPException) as exc_info:
        assert ensure_metadata_labels({"opensandbox.io/manual-cleanup": "true"}) is None
    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL
    assert "reserved prefix" in exc_info.value.detail["message"]

def test_ensure_metadata_labels_rejects_arbitrary_reserved_key():
    """Any key under opensandbox.io/ should be rejected, not just known labels."""
    with pytest.raises(HTTPException) as exc_info:
        assert ensure_metadata_labels({"opensandbox.io/custom": "value"}) is None
    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL

def test_ensure_timeout_within_limit_allows_equal_boundary():
    assert ensure_timeout_within_limit(3600, 3600) is None

def test_ensure_timeout_within_limit_allows_disabled_upper_bound():
    assert ensure_timeout_within_limit(7200, None) is None

def test_ensure_timeout_within_limit_rejects_timeout_above_limit():
    with pytest.raises(HTTPException) as exc_info:
        assert ensure_timeout_within_limit(3601, 3600) is None

    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER

def test_ensure_timeout_within_limit_rejects_unrepresentable_timeout():
    with pytest.raises(HTTPException) as exc_info:
        ensure_timeout_within_limit(10**20, None)

    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
    assert "too large" in exc_info.value.detail["message"]

class TestEnsureValidVolumeName:

    def test_valid_simple_name(self):
        """Simple lowercase names should be valid."""
        assert ensure_valid_volume_name("workdir") is None
        assert ensure_valid_volume_name("data") is None
        assert ensure_valid_volume_name("models") is None

    def test_valid_name_with_numbers(self):
        """Names with numbers should be valid."""
        assert ensure_valid_volume_name("data1") is None
        assert ensure_valid_volume_name("vol2") is None
        assert ensure_valid_volume_name("123") is None

    def test_valid_name_with_hyphens(self):
        """Names with hyphens should be valid."""
        assert ensure_valid_volume_name("my-volume") is None
        assert ensure_valid_volume_name("data-cache-1") is None
        assert ensure_valid_volume_name("a-b-c") is None

    def test_empty_name_raises(self):
        """Empty name should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_volume_name("")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_VOLUME_NAME

    def test_name_too_long_raises(self):
        """Name exceeding 63 characters should raise HTTPException."""
        long_name = "a" * 64
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_volume_name(long_name)
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_VOLUME_NAME

    def test_uppercase_name_raises(self):
        """Uppercase letters should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_volume_name("MyVolume")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_VOLUME_NAME

    def test_underscore_name_raises(self):
        """Underscores should raise HTTPException (not valid DNS label)."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_volume_name("my_volume")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_VOLUME_NAME

    def test_name_starting_with_hyphen_raises(self):
        """Names starting with hyphen should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_volume_name("-volume")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_VOLUME_NAME

    def test_name_ending_with_hyphen_raises(self):
        """Names ending with hyphen should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_volume_name("volume-")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_VOLUME_NAME

class TestEnsureValidMountPath:

    def test_valid_absolute_path(self):
        """Absolute paths should be valid."""
        assert ensure_valid_mount_path("/mnt/data") is None
        assert ensure_valid_mount_path("/") is None
        assert ensure_valid_mount_path("/home/user/work") is None

    def test_empty_path_raises(self):
        """Empty path should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_mount_path("")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_MOUNT_PATH

    def test_relative_path_raises(self):
        """Relative paths should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_mount_path("data/files")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_MOUNT_PATH

    def test_path_not_starting_with_slash_raises(self):
        """Paths not starting with '/' should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_mount_path("mnt/data")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_MOUNT_PATH

class TestEnsureValidSubPath:

    def test_none_subpath_valid(self):
        """None subpath should be valid."""
        assert ensure_valid_sub_path(None) is None

    def test_empty_subpath_valid(self):
        """Empty string subpath should be valid."""
        assert ensure_valid_sub_path("") is None

    def test_relative_subpath_valid(self):
        """Relative paths should be valid."""
        assert ensure_valid_sub_path("task-001") is None
        assert ensure_valid_sub_path("user/data") is None
        assert ensure_valid_sub_path("a/b/c") is None

    def test_absolute_subpath_raises(self):
        """Absolute paths should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_sub_path("/absolute/path")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_SUB_PATH

    def test_path_traversal_raises(self):
        """Path traversal (..) should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_sub_path("../parent")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_SUB_PATH

    def test_embedded_path_traversal_raises(self):
        """Embedded path traversal should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_sub_path("a/../b")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_SUB_PATH

class TestEnsureValidHostPath:

    def test_valid_absolute_path(self):
        """Absolute paths should be valid."""
        assert ensure_valid_host_path("/data/opensandbox") is None
        assert ensure_valid_host_path("/tmp") is None

    def test_valid_windows_absolute_path(self):
        """Windows absolute paths should be valid."""
        assert ensure_valid_host_path(r"D:\sandbox-mnt\ReMe") is None
        assert ensure_valid_host_path("D:/sandbox-mnt/ReMe") is None

    def test_valid_windows_drive_root(self):
        """Windows drive roots should be valid absolute paths."""
        assert ensure_valid_host_path("D:\\") is None
        assert ensure_valid_host_path("D:/") is None

    def test_empty_path_raises(self):
        """Empty path should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_host_path("")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_HOST_PATH

    def test_relative_path_raises(self):
        """Relative paths should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_host_path("data/files")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_HOST_PATH

    def test_path_with_traversal_raises(self):
        """Paths with traversal should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_host_path("/data/../etc/passwd")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_HOST_PATH

    def test_path_with_double_slash_raises(self):
        """Paths with double slashes should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_host_path("/data//files")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_HOST_PATH

    def test_allowed_prefix_match(self):
        """Paths under allowed prefixes should be valid."""
        allowed = ["/data/opensandbox", "/tmp/sandbox"]
        assert ensure_valid_host_path("/data/opensandbox/user-a", allowed) is None
        assert ensure_valid_host_path("/tmp/sandbox/task-1", allowed) is None

    def test_allowed_prefix_exact_match(self):
        """Exact prefix match should be valid."""
        allowed = ["/data/opensandbox"]
        assert ensure_valid_host_path("/data/opensandbox", allowed) is None

    def test_allowed_prefix_match_windows_paths(self):
        """Windows paths under an allowed Windows prefix should be valid."""
        allowed = [r"D:\sandbox-mnt"]
        assert ensure_valid_host_path(r"D:\sandbox-mnt\ReMe", allowed) is None
        assert ensure_valid_host_path("D:/sandbox-mnt/ReMe", allowed) is None

    def test_allowed_prefix_match_windows_paths_is_case_insensitive_for_drive(self):
        """Drive-letter casing differences should not break allowlist checks."""
        allowed = ["D:/sandbox-mnt"]
        assert ensure_valid_host_path("d:/sandbox-mnt/ReMe", allowed) is None

    def test_path_not_in_allowed_prefix_raises(self):
        """Paths not under allowed prefixes should raise HTTPException."""
        allowed = ["/data/opensandbox"]
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_host_path("/etc/passwd", allowed)
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.HOST_PATH_NOT_ALLOWED

    def test_partial_prefix_match_raises(self):
        """Partial prefix matches should not be allowed."""
        allowed = ["/data/opensandbox"]
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_host_path("/data/opensandbox-evil", allowed)
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.HOST_PATH_NOT_ALLOWED

class TestEnsureValidPvcName:

    def test_valid_simple_name(self):
        """Simple lowercase names should be valid."""
        assert ensure_valid_pvc_name("my-pvc") is None
        assert ensure_valid_pvc_name("data-volume") is None
        assert ensure_valid_pvc_name("pvc1") is None

    def test_empty_name_raises(self):
        """Empty name should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_pvc_name("")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PVC_NAME

    def test_name_too_long_raises(self):
        """Name exceeding 253 characters should raise HTTPException."""
        long_name = "a" * 254
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_pvc_name(long_name)
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PVC_NAME

    def test_uppercase_name_raises(self):
        """Uppercase letters should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_pvc_name("MyPVC")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PVC_NAME

    def test_underscore_name_raises(self):
        """Underscores should raise HTTPException."""
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_pvc_name("my_pvc")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PVC_NAME

class TestEnsureVolumesValid:

    def test_none_volumes_valid(self):
        """None volumes should be valid."""
        assert ensure_volumes_valid(None) is None

    def test_empty_volumes_valid(self):
        """Empty volumes list should be valid."""
        assert ensure_volumes_valid([]) is None

    def test_valid_host_volume(self):
        """Valid host volume should pass validation."""
        volume = Volume(
            name="workdir",
            host=Host(path="/data/opensandbox"),
            mount_path="/mnt/work",
            read_only=False,
        )
        assert ensure_volumes_valid([volume]) is None

    def test_valid_pvc_volume(self):
        """Valid PVC volume should pass validation."""
        volume = Volume(
            name="models",
            pvc=PVC(claim_name="shared-models-pvc"),
            mount_path="/mnt/models",
            read_only=True,
        )
        assert ensure_volumes_valid([volume]) is None

    def test_valid_ossfs_volume(self):
        """Valid OSSFS volume should pass validation."""
        volume = Volume(
            name="oss-data",
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
        assert ensure_volumes_valid([volume]) is None

    def test_valid_volume_with_subpath(self):
        """Valid volume with subPath should pass validation."""
        volume = Volume(
            name="workdir",
            host=Host(path="/data/opensandbox"),
            mount_path="/mnt/work",
            read_only=False,
            sub_path="task-001",
        )
        assert ensure_volumes_valid([volume]) is None

    def test_multiple_valid_volumes(self):
        """Multiple valid volumes should pass validation."""
        volumes = [
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
        ]
        assert ensure_volumes_valid(volumes) is None

    def test_duplicate_volume_name_raises(self):
        """Duplicate volume names should raise HTTPException."""
        volumes = [
            Volume(
                name="workdir",
                host=Host(path="/data/a"),
                mount_path="/mnt/a",
                read_only=False,
            ),
            Volume(
                name="workdir",  # Duplicate name
                host=Host(path="/data/b"),
                mount_path="/mnt/b",
                read_only=False,
            ),
        ]
        with pytest.raises(HTTPException) as exc_info:
            ensure_volumes_valid(volumes)
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.DUPLICATE_VOLUME_NAME

    def test_invalid_volume_name_rejected_by_pydantic(self):
        """Invalid volume name should be rejected by Pydantic pattern validation."""
        from pydantic import ValidationError

        # Pydantic validates the pattern before our validators run
        with pytest.raises(ValidationError) as exc_info:
            Volume(
                name="Invalid_Name",  # Invalid: uppercase and underscore
                host=Host(path="/data/opensandbox"),
                mount_path="/mnt/work",
                read_only=False,
            )
        assert "name" in str(exc_info.value)

    def test_invalid_mount_path_rejected_by_pydantic(self):
        """Invalid mount path should be rejected by Pydantic pattern validation."""
        from pydantic import ValidationError

        # Pydantic validates the pattern before our validators run
        with pytest.raises(ValidationError) as exc_info:
            Volume(
                name="workdir",
                host=Host(path="/data/opensandbox"),
                mount_path="relative/path",  # Invalid: not absolute
                read_only=False,
            )
        assert "mount_path" in str(exc_info.value)

    def test_invalid_subpath_raises(self):
        """Invalid subPath should raise HTTPException."""
        volume = Volume(
            name="workdir",
            host=Host(path="/data/opensandbox"),
            mount_path="/mnt/work",
            read_only=False,
            sub_path="../escape",  # Invalid: path traversal
        )
        with pytest.raises(HTTPException) as exc_info:
            ensure_volumes_valid([volume])
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_SUB_PATH

    def test_host_path_allowlist_enforced(self):
        """Host path allowlist should be enforced."""
        volume = Volume(
            name="workdir",
            host=Host(path="/etc/passwd"),  # Not in allowed list
            mount_path="/mnt/work",
            read_only=False,
        )
        with pytest.raises(HTTPException) as exc_info:
            ensure_volumes_valid([volume], allowed_host_prefixes=["/data/opensandbox"])
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.HOST_PATH_NOT_ALLOWED

    def test_ossfs_invalid_version_rejected_by_schema(self):
        """Unsupported OSSFS version should be rejected by schema validation."""
        from pydantic import ValidationError

        with pytest.raises(ValidationError):
            OSSFS(
                bucket="bucket-test-3",
                endpoint="oss-cn-hangzhou.aliyuncs.com",
                version="3.0",  # type: ignore[arg-type]
                access_key_id="AKIDEXAMPLE",
                access_key_secret="SECRETEXAMPLE",
            )

    def test_ossfs_missing_inline_credentials_raises(self):
        """Missing inline credentials should raise HTTPException."""
        volume = Volume(
            name="oss-data",
            ossfs=OSSFS(
                bucket="bucket-test-3",
                endpoint="oss-cn-hangzhou.aliyuncs.com",
                access_key_id="AKIDEXAMPLE",
                access_key_secret="SECRETEXAMPLE",
            ),
            mount_path="/mnt/data",
        )
        volume.ossfs.access_key_id = None
        with pytest.raises(HTTPException) as exc_info:
            ensure_volumes_valid([volume])
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_OSSFS_CREDENTIALS

    def test_ossfs_v1_options_reject_prefixed_entries(self):
        """OSSFS options should reject prefixed entries for 1.0."""
        volume = Volume(
            name="oss-data",
            ossfs=OSSFS(
                bucket="bucket-test-3",
                endpoint="oss-cn-hangzhou.aliyuncs.com",
                version="1.0",
                options=["--allow_other"],
                access_key_id="AKIDEXAMPLE",
                access_key_secret="SECRETEXAMPLE",
            ),
            mount_path="/mnt/data",
        )
        with pytest.raises(HTTPException) as exc_info:
            ensure_volumes_valid([volume])
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_OSSFS_OPTION

    def test_ossfs_v2_options_reject_prefixed_entries(self):
        """OSSFS options should reject prefixed entries for 2.0."""
        volume = Volume(
            name="oss-data",
            ossfs=OSSFS(
                bucket="bucket-test-3",
                endpoint="oss-cn-hangzhou.aliyuncs.com",
                version="2.0",
                options=["-o allow_other"],
                access_key_id="AKIDEXAMPLE",
                access_key_secret="SECRETEXAMPLE",
            ),
            mount_path="/mnt/data",
        )
        with pytest.raises(HTTPException) as exc_info:
            ensure_volumes_valid([volume])
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_OSSFS_OPTION

    def test_invalid_pvc_name_rejected_by_pydantic(self):
        """Invalid PVC name should be rejected by Pydantic pattern validation."""
        from pydantic import ValidationError

        # Pydantic validates the pattern before our validators run
        with pytest.raises(ValidationError) as exc_info:
            PVC(claim_name="Invalid_PVC")  # Invalid: uppercase and underscore
        assert "claim_name" in str(exc_info.value)
