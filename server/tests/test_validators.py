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

import pytest
from fastapi import HTTPException

from opensandbox_server.api.schema import Host, OSSFS, PVC, Volume, PlatformSpec
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.validators import (
    ensure_credential_proxy_configured,
    ensure_egress_runtime_compatible,
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
    long_name = "a" * 64
    with pytest.raises(HTTPException) as exc_info:
        assert ensure_metadata_labels({long_name: "value"}) is None
    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL

def test_ensure_metadata_labels_rejects_prefix_too_long():
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
    with pytest.raises(HTTPException) as exc_info:
        assert ensure_metadata_labels({"INVALID_PREFIX.io/name": "value"}) is None
    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL

def test_ensure_metadata_labels_rejects_value_too_long():
    long_value = "a" * 64
    with pytest.raises(HTTPException) as exc_info:
        assert ensure_metadata_labels({"app": long_value}) is None
    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL

def test_ensure_metadata_labels_rejects_key_with_empty_prefix():
    with pytest.raises(HTTPException) as exc_info:
        assert ensure_metadata_labels({"/name": "value"}) is None
    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL

def test_ensure_metadata_labels_rejects_reserved_prefix():
    with pytest.raises(HTTPException) as exc_info:
        assert ensure_metadata_labels({"opensandbox.io/expires-at": "2030-01-01T00:00:00Z"}) is None
    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL
    assert "reserved prefix" in exc_info.value.detail["message"]

def test_ensure_metadata_labels_rejects_manual_cleanup_key():
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
        assert ensure_valid_volume_name("workdir") is None
        assert ensure_valid_volume_name("data") is None
        assert ensure_valid_volume_name("models") is None

    def test_valid_name_with_numbers(self):
        assert ensure_valid_volume_name("data1") is None
        assert ensure_valid_volume_name("vol2") is None
        assert ensure_valid_volume_name("123") is None

    def test_valid_name_with_hyphens(self):
        assert ensure_valid_volume_name("my-volume") is None
        assert ensure_valid_volume_name("data-cache-1") is None
        assert ensure_valid_volume_name("a-b-c") is None

    def test_empty_name_raises(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_volume_name("")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_VOLUME_NAME

    def test_name_too_long_raises(self):
        long_name = "a" * 64
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_volume_name(long_name)
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_VOLUME_NAME

    def test_uppercase_name_raises(self):
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
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_volume_name("-volume")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_VOLUME_NAME

    def test_name_ending_with_hyphen_raises(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_volume_name("volume-")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_VOLUME_NAME

class TestEnsureValidMountPath:

    def test_valid_absolute_path(self):
        assert ensure_valid_mount_path("/mnt/data") is None
        assert ensure_valid_mount_path("/") is None
        assert ensure_valid_mount_path("/home/user/work") is None

    def test_empty_path_raises(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_mount_path("")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_MOUNT_PATH

    def test_relative_path_raises(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_mount_path("data/files")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_MOUNT_PATH

    def test_path_not_starting_with_slash_raises(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_mount_path("mnt/data")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_MOUNT_PATH

class TestEnsureValidSubPath:

    def test_none_subpath_valid(self):
        assert ensure_valid_sub_path(None) is None

    def test_empty_subpath_valid(self):
        assert ensure_valid_sub_path("") is None

    def test_relative_subpath_valid(self):
        assert ensure_valid_sub_path("task-001") is None
        assert ensure_valid_sub_path("user/data") is None
        assert ensure_valid_sub_path("a/b/c") is None

    def test_absolute_subpath_raises(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_sub_path("/absolute/path")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_SUB_PATH

    def test_path_traversal_raises(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_sub_path("../parent")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_SUB_PATH

    def test_embedded_path_traversal_raises(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_sub_path("a/../b")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_SUB_PATH

class TestEnsureValidHostPath:

    def test_valid_absolute_path(self):
        assert ensure_valid_host_path("/data/opensandbox") is None
        assert ensure_valid_host_path("/tmp") is None

    def test_valid_windows_absolute_path(self):
        assert ensure_valid_host_path(r"D:\sandbox-mnt\ReMe") is None
        assert ensure_valid_host_path("D:/sandbox-mnt/ReMe") is None

    def test_valid_windows_drive_root(self):
        assert ensure_valid_host_path("D:\\") is None
        assert ensure_valid_host_path("D:/") is None

    def test_empty_path_raises(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_host_path("")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_HOST_PATH

    def test_relative_path_raises(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_host_path("data/files")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_HOST_PATH

    def test_path_with_traversal_raises(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_host_path("/data/../etc/passwd")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_HOST_PATH

    def test_path_with_double_slash_raises(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_host_path("/data//files")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_HOST_PATH

    def test_allowed_prefix_match(self):
        allowed = ["/data/opensandbox", "/tmp/sandbox"]
        assert ensure_valid_host_path("/data/opensandbox/user-a", allowed) is None
        assert ensure_valid_host_path("/tmp/sandbox/task-1", allowed) is None

    def test_allowed_prefix_exact_match(self):
        allowed = ["/data/opensandbox"]
        assert ensure_valid_host_path("/data/opensandbox", allowed) is None

    def test_allowed_prefix_match_windows_paths(self):
        allowed = [r"D:\sandbox-mnt"]
        assert ensure_valid_host_path(r"D:\sandbox-mnt\ReMe", allowed) is None
        assert ensure_valid_host_path("D:/sandbox-mnt/ReMe", allowed) is None

    def test_allowed_prefix_match_windows_paths_is_case_insensitive_for_drive(self):
        allowed = ["D:/sandbox-mnt"]
        assert ensure_valid_host_path("d:/sandbox-mnt/ReMe", allowed) is None

    def test_path_not_in_allowed_prefix_raises(self):
        allowed = ["/data/opensandbox"]
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_host_path("/etc/passwd", allowed)
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.HOST_PATH_NOT_ALLOWED

    def test_partial_prefix_match_raises(self):
        allowed = ["/data/opensandbox"]
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_host_path("/data/opensandbox-evil", allowed)
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.HOST_PATH_NOT_ALLOWED

class TestEnsureValidPvcName:

    def test_valid_simple_name(self):
        assert ensure_valid_pvc_name("my-pvc") is None
        assert ensure_valid_pvc_name("data-volume") is None
        assert ensure_valid_pvc_name("pvc1") is None

    def test_empty_name_raises(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_pvc_name("")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PVC_NAME

    def test_name_too_long_raises(self):
        long_name = "a" * 254
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_pvc_name(long_name)
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PVC_NAME

    def test_uppercase_name_raises(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_pvc_name("MyPVC")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PVC_NAME

    def test_underscore_name_raises(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_valid_pvc_name("my_pvc")
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PVC_NAME

class TestEnsureVolumesValid:

    def test_none_volumes_valid(self):
        assert ensure_volumes_valid(None) is None

    def test_empty_volumes_valid(self):
        assert ensure_volumes_valid([]) is None

    def test_valid_host_volume(self):
        volume = Volume(
            name="workdir",
            host=Host(path="/data/opensandbox"),
            mount_path="/mnt/work",
            read_only=False,
        )
        assert ensure_volumes_valid([volume]) is None

    def test_valid_pvc_volume(self):
        volume = Volume(
            name="models",
            pvc=PVC(claim_name="shared-models-pvc"),
            mount_path="/mnt/models",
            read_only=True,
        )
        assert ensure_volumes_valid([volume]) is None

    def test_valid_ossfs_volume(self):
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
        volume = Volume(
            name="workdir",
            host=Host(path="/data/opensandbox"),
            mount_path="/mnt/work",
            read_only=False,
            sub_path="task-001",
        )
        assert ensure_volumes_valid([volume]) is None

    def test_multiple_valid_volumes(self):
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
        volumes = [
            Volume(
                name="workdir",
                host=Host(path="/data/a"),
                mount_path="/mnt/a",
                read_only=False,
            ),
            Volume(
                name="workdir",
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
        from pydantic import ValidationError

        # Pydantic validates the pattern before our validators run
        with pytest.raises(ValidationError) as exc_info:
            Volume(
                name="Invalid_Name",
                host=Host(path="/data/opensandbox"),
                mount_path="/mnt/work",
                read_only=False,
            )
        assert "name" in str(exc_info.value)

    def test_invalid_mount_path_rejected_by_pydantic(self):
        from pydantic import ValidationError

        # Pydantic validates the pattern before our validators run
        with pytest.raises(ValidationError) as exc_info:
            Volume(
                name="workdir",
                host=Host(path="/data/opensandbox"),
                mount_path="relative/path",
                read_only=False,
            )
        assert "mount_path" in str(exc_info.value)

    def test_invalid_subpath_raises(self):
        volume = Volume(
            name="workdir",
            host=Host(path="/data/opensandbox"),
            mount_path="/mnt/work",
            read_only=False,
            sub_path="../escape",
        )
        with pytest.raises(HTTPException) as exc_info:
            ensure_volumes_valid([volume])
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_SUB_PATH

    def test_host_path_allowlist_enforced(self):
        volume = Volume(
            name="workdir",
            host=Host(path="/etc/passwd"),
            mount_path="/mnt/work",
            read_only=False,
        )
        with pytest.raises(HTTPException) as exc_info:
            ensure_volumes_valid([volume], allowed_host_prefixes=["/data/opensandbox"])
        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.HOST_PATH_NOT_ALLOWED

    def test_ossfs_invalid_version_rejected_by_schema(self):
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
        from pydantic import ValidationError

        # Pydantic validates the pattern before our validators run
        with pytest.raises(ValidationError) as exc_info:
            PVC(claim_name="Invalid_PVC")
        assert "claim_name" in str(exc_info.value)


class TestCredentialProxyConfiguration:
    def test_allows_dns_nft_mode(self):
        from opensandbox_server.api.schema import CredentialProxyConfig, NetworkPolicy
        from opensandbox_server.config import EgressConfig

        ensure_credential_proxy_configured(
            CredentialProxyConfig(enabled=True),
            NetworkPolicy(default_action="deny", egress=[]),
            EgressConfig(image="egress:latest", mode="dns+nft"),
        )

    def test_warns_for_default_allow_policy(self, monkeypatch):
        from opensandbox_server.api.schema import CredentialProxyConfig, NetworkPolicy
        from opensandbox_server.config import EgressConfig

        warnings = []
        monkeypatch.setattr(
            "opensandbox_server.services.validators.logger.warning",
            lambda message, *args: warnings.append(message % args),
        )

        ensure_credential_proxy_configured(
            CredentialProxyConfig(enabled=True),
            NetworkPolicy(default_action="allow", egress=[]),
            EgressConfig(image="egress:latest", mode="dns+nft"),
        )

        assert len(warnings) == 1
        assert "allowed for backward compatibility but is deprecated" in warnings[0]

    def test_rejects_dns_only_mode(self):
        from opensandbox_server.api.schema import CredentialProxyConfig, NetworkPolicy
        from opensandbox_server.config import EgressConfig

        with pytest.raises(HTTPException) as exc_info:
            ensure_credential_proxy_configured(
                CredentialProxyConfig(enabled=True),
                NetworkPolicy(default_action="deny", egress=[]),
                EgressConfig(image="egress:latest", mode="dns"),
            )

        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
        assert "dns+nft" in exc_info.value.detail["message"]

    def test_ignores_disabled_proxy(self):
        from opensandbox_server.api.schema import CredentialProxyConfig

        ensure_credential_proxy_configured(
            CredentialProxyConfig(enabled=False), None, None
        )


class TestEgressRuntimeCompatibility:

    def _network_policy(self):
        from opensandbox_server.api.schema import NetworkPolicy
        return NetworkPolicy(default_action="deny", egress=[])

    def _secure_runtime(self, type_: str):
        from opensandbox_server.config import SecureRuntimeConfig
        if type_ == "gvisor":
            return SecureRuntimeConfig(type=type_, k8s_runtime_class="gvisor")
        if type_ == "kata":
            return SecureRuntimeConfig(type=type_, k8s_runtime_class="kata-qemu")
        return SecureRuntimeConfig(type=type_)

    def test_rejects_gvisor_with_network_policy(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_egress_runtime_compatible(self._network_policy(), self._secure_runtime("gvisor"))
        assert exc_info.value.status_code == 400
        assert "gVisor" in exc_info.value.detail["message"]
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER

    def test_allows_kata_with_network_policy(self):
        ensure_egress_runtime_compatible(self._network_policy(), self._secure_runtime("kata"))

    def test_allows_no_secure_runtime(self):
        ensure_egress_runtime_compatible(self._network_policy(), None)

    def test_allows_empty_secure_runtime(self):
        ensure_egress_runtime_compatible(self._network_policy(), self._secure_runtime(""))

    def test_allows_gvisor_without_network_policy(self):
        ensure_egress_runtime_compatible(None, self._secure_runtime("gvisor"))

    def test_rejects_template_gvisor_with_network_policy(self):
        with pytest.raises(HTTPException) as exc_info:
            ensure_egress_runtime_compatible(
                self._network_policy(), None, effective_runtime_class="gvisor"
            )
        assert exc_info.value.status_code == 400
        assert "gVisor" in exc_info.value.detail["message"]

    def test_allows_template_kata_with_network_policy(self):
        ensure_egress_runtime_compatible(
            self._network_policy(), None, effective_runtime_class="kata-qemu"
        )

    def test_secure_runtime_takes_precedence_over_template(self):
        with pytest.raises(HTTPException):
            ensure_egress_runtime_compatible(
                self._network_policy(),
                self._secure_runtime("gvisor"),
                effective_runtime_class="kata-qemu",
            )
