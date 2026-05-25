#
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
#
from __future__ import annotations

from datetime import datetime, timezone

import pytest

from opensandbox.api.lifecycle.models.create_sandbox_response import (
    CreateSandboxResponse as ApiCreateSandboxResponse,
)
from opensandbox.api.lifecycle.models.image_spec import ImageSpec as ApiImageSpec
from opensandbox.api.lifecycle.models.sandbox import Sandbox as ApiSandbox
from opensandbox.api.lifecycle.types import UNSET
from opensandbox.models.execd import (
    Execution,
    ExecutionError,
    ExecutionLogs,
    ExecutionResult,
    OutputMessage,
)
from opensandbox.models.filesystem import MoveEntry, WriteEntry
from opensandbox.models.sandboxes import (
    OSSFS,
    PVC,
    Host,
    SandboxFilter,
    SandboxImageAuth,
    SandboxImageSpec,
    SandboxInfo,
    SandboxStatus,
    Volume,
)


def test_sandbox_image_spec_supports_positional_image() -> None:
    spec = SandboxImageSpec("python:3.11")
    assert spec.image == "python:3.11"


def test_sandbox_image_spec_rejects_blank_image() -> None:
    with pytest.raises(ValueError):
        SandboxImageSpec("   ")


def test_api_image_spec_tolerates_omitted_auth() -> None:
    spec = ApiImageSpec.from_dict({"uri": "python:3.11"})
    assert spec.uri == "python:3.11"
    assert spec.auth is UNSET


def test_api_create_sandbox_response_tolerates_omitted_optional_fields() -> None:
    response = ApiCreateSandboxResponse.from_dict(
        {
            "id": "sandbox-1",
            "status": {"state": "Running"},
            "createdAt": "2025-01-01T00:00:00Z",
            "entrypoint": ["/bin/sh"],
        }
    )
    assert response.metadata is UNSET
    assert response.expires_at is UNSET
    assert response.status.last_transition_at is UNSET


def test_api_sandbox_tolerates_omitted_optional_fields() -> None:
    sandbox = ApiSandbox.from_dict(
        {
            "id": "sandbox-1",
            "image": {"uri": "python:3.11"},
            "status": {"state": "Running"},
            "entrypoint": ["/bin/sh"],
            "createdAt": "2025-01-01T00:00:00Z",
        }
    )
    assert sandbox.metadata is UNSET
    assert sandbox.expires_at is UNSET
    assert sandbox.status.last_transition_at is UNSET


def test_sandbox_image_auth_rejects_blank_username_and_password() -> None:
    with pytest.raises(ValueError):
        SandboxImageAuth(username=" ", password="x")
    with pytest.raises(ValueError):
        SandboxImageAuth(username="u", password=" ")


def test_sandbox_filter_validations() -> None:
    SandboxFilter(page=0, page_size=1)
    with pytest.raises(ValueError):
        SandboxFilter(page=-1)
    with pytest.raises(ValueError):
        SandboxFilter(page_size=0)


def test_sandbox_status_and_info_alias_dump_is_stable() -> None:
    status = SandboxStatus(
        state="RUNNING", last_transition_at=datetime(2025, 1, 1, tzinfo=timezone.utc)
    )
    info = SandboxInfo(
        id=str(__import__("uuid").uuid4()),
        status=status,
        entrypoint=["/bin/sh"],
        expires_at=datetime(2025, 1, 2, tzinfo=timezone.utc),
        created_at=datetime(2025, 1, 1, tzinfo=timezone.utc),
        image=SandboxImageSpec("python:3.11"),
        metadata={"k": "v"},
    )

    dumped = info.model_dump(by_alias=True, mode="json")
    assert "expires_at" in dumped
    assert "created_at" in dumped
    assert dumped["status"]["last_transition_at"].endswith(("Z", "+00:00"))


def test_sandbox_info_supports_manual_cleanup_expiration() -> None:
    info = SandboxInfo(
        id=str(__import__("uuid").uuid4()),
        status=SandboxStatus(state="RUNNING"),
        entrypoint=["/bin/sh"],
        expires_at=None,
        created_at=datetime(2025, 1, 1, tzinfo=timezone.utc),
        image=SandboxImageSpec("python:3.11"),
    )

    dumped = info.model_dump(by_alias=True, mode="json")
    assert dumped["expires_at"] is None


def test_filesystem_models_aliases_and_validation() -> None:
    m = MoveEntry(source="/a", destination="/b")
    assert m.src == "/a"
    assert m.dest == "/b"

    with pytest.raises(ValueError):
        WriteEntry(path="  ", data="x")


# ============================================================================
# Volume Model Tests
# ============================================================================


def test_host_backend_requires_absolute_path() -> None:
    backend = Host(path="/data/shared")
    assert backend.path == "/data/shared"

    with pytest.raises(ValueError, match="absolute path"):
        Host(path="relative/path")

def test_host_backend_accepts_unix_root_path() -> None:
    """Unix root path '/' must be accepted."""
    assert Host(path="/").path == "/"


def test_host_backend_accepts_unix_nested_path() -> None:
    """Unix nested absolute path must be accepted."""
    assert Host(path="/mnt/host/project").path == "/mnt/host/project"


def test_host_backend_accepts_windows_backslash_path() -> None:
    """Windows drive path with backslashes must be accepted."""
    backend = Host(path="D:\\sandbox-mnt\\ReMe")
    assert backend.path == "D:\\sandbox-mnt\\ReMe"


def test_host_backend_accepts_windows_forward_slash_path() -> None:
    """Windows drive path with forward slashes must be accepted."""
    backend = Host(path="D:/sandbox-mnt/ReMe")
    assert backend.path == "D:/sandbox-mnt/ReMe"


def test_host_backend_accepts_windows_drive_root() -> None:
    """Windows drive root (e.g. 'Z:\\') must be accepted."""
    assert Host(path="Z:\\").path == "Z:\\"


def test_host_backend_accepts_windows_lowercase_drive() -> None:
    """Lowercase drive letter must be accepted."""
    assert Host(path="a:/lower").path == "a:/lower"


def test_host_backend_rejects_relative_path() -> None:
    """Relative path without leading separator must be rejected."""
    with pytest.raises(ValueError, match="absolute path"):
        Host(path="relative/path")


def test_host_backend_rejects_dot_relative_path() -> None:
    """Dot-relative paths must be rejected."""
    with pytest.raises(ValueError, match="absolute path"):
        Host(path="./local")


def test_host_backend_rejects_parent_traversal_path() -> None:
    """Parent-traversal paths must be rejected."""
    with pytest.raises(ValueError, match="absolute path"):
        Host(path="../parent")


def test_host_backend_rejects_empty_path() -> None:
    """Empty string must be rejected."""
    with pytest.raises(ValueError, match="absolute path"):
        Host(path="")

def test_pvc_backend_rejects_blank_claim_name() -> None:
    backend = PVC(claimName="my-pvc")
    assert backend.claim_name == "my-pvc"

    with pytest.raises(ValueError, match="blank"):
        PVC(claimName="   ")


def test_ossfs_backend_default_version_is_2_0() -> None:
    backend = OSSFS(
        bucket="bucket-test-3",
        endpoint="oss-cn-hangzhou.aliyuncs.com",
        accessKeyId="ak",
        accessKeySecret="sk",
    )
    assert backend.version == "2.0"


def test_volume_with_host_backend() -> None:
    vol = Volume(
        name="data",
        host=Host(path="/data/shared"),
        mountPath="/mnt/data",
    )
    assert vol.name == "data"
    assert vol.host is not None
    assert vol.host.path == "/data/shared"
    assert vol.pvc is None
    assert vol.mount_path == "/mnt/data"
    assert vol.read_only is False  # default is read-write
    assert vol.sub_path is None


def test_volume_with_pvc_backend() -> None:
    vol = Volume(
        name="models",
        pvc=PVC(claimName="shared-models"),
        mountPath="/mnt/models",
        readOnly=True,
        subPath="v1",
    )
    assert vol.name == "models"
    assert vol.host is None
    assert vol.pvc is not None
    assert vol.pvc.claim_name == "shared-models"
    assert vol.mount_path == "/mnt/models"
    assert vol.read_only is True
    assert vol.sub_path == "v1"


def test_volume_rejects_blank_name() -> None:
    with pytest.raises(ValueError, match="blank"):
        Volume(
            name="   ",
            host=Host(path="/data"),
            mountPath="/mnt",
        )


def test_volume_requires_absolute_mount_path() -> None:
    with pytest.raises(ValueError, match="absolute path"):
        Volume(
            name="test",
            host=Host(path="/data"),
            mountPath="relative/path",
        )


def test_volume_serialization_uses_aliases() -> None:
    vol = Volume(
        name="test",
        pvc=PVC(claimName="my-pvc"),
        mountPath="/mnt/test",
        readOnly=True,
        subPath="sub",
    )
    dumped = vol.model_dump(by_alias=True, mode="json")
    assert "mountPath" in dumped
    assert "readOnly" in dumped
    assert "subPath" in dumped
    assert dumped["pvc"]["claimName"] == "my-pvc"
    assert dumped["readOnly"] is True


def test_volume_rejects_no_backend() -> None:
    """Volume must have exactly one backend specified."""
    with pytest.raises(ValueError, match="none was provided"):
        Volume(
            name="test",
            mountPath="/mnt/test",
        )


def test_volume_rejects_multiple_backends() -> None:
    """Volume must have exactly one backend, not multiple."""
    with pytest.raises(ValueError, match="multiple were provided"):
        Volume(
            name="test",
            host=Host(path="/data"),
            pvc=PVC(claimName="my-pvc"),
            mountPath="/mnt/test",
        )


# ============================================================================
# Execution __str__ and .text Tests
# ============================================================================


def _make_output(text: str, *, is_error: bool = False) -> OutputMessage:
    return OutputMessage(text=text, timestamp=0, is_error=is_error)


def _make_result(text: str) -> ExecutionResult:
    return ExecutionResult(text=text, timestamp=0)


def test_execution_str_stdout_only() -> None:
    ex = Execution(
        logs=ExecutionLogs(
            stdout=[_make_output("hello"), _make_output("world")],
        ),
    )
    assert str(ex) == "hello\nworld"


def test_execution_str_with_stderr() -> None:
    ex = Execution(
        logs=ExecutionLogs(
            stdout=[_make_output("ok")],
            stderr=[_make_output("warn", is_error=True)],
        ),
    )
    assert str(ex) == "ok\n[stderr]\nwarn"


def test_execution_str_with_error() -> None:
    ex = Execution(
        error=ExecutionError(name="RuntimeError", value="boom", timestamp=0),
    )
    assert str(ex) == "[error] RuntimeError: boom"


def test_execution_str_empty() -> None:
    ex = Execution()
    assert str(ex) == ""
    assert ex.complete is None
    assert ex.exit_code is None


def test_execution_text_property() -> None:
    ex = Execution(
        logs=ExecutionLogs(
            stdout=[_make_output("line1"), _make_output("line2")],
            stderr=[_make_output("ignored", is_error=True)],
        ),
    )
    assert ex.text == "line1\nline2"


def test_execution_text_includes_results() -> None:
    """code-interpreter stores return values in result, not stdout."""
    ex = Execution(
        result=[_make_result("4")],
    )
    assert ex.text == "4"
    assert str(ex) == "4"


def test_execution_text_combines_stdout_and_results() -> None:
    ex = Execution(
        logs=ExecutionLogs(
            stdout=[_make_output("3.11.14")],
        ),
        result=[_make_result("4")],
    )
    assert ex.text == "3.11.14\n4"


def test_execution_text_strips_trailing_newlines() -> None:
    """code-interpreter streaming sends chunks with trailing newlines."""
    ex = Execution(
        logs=ExecutionLogs(
            stdout=[_make_output("1\n"), _make_output("2\n")],
        ),
    )
    assert ex.text == "1\n2"
    assert str(ex) == "1\n2"
