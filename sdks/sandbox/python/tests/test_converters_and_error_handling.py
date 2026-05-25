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

from datetime import datetime, timedelta

import pytest
from httpx import HTTPStatusError, Request, Response

from opensandbox.adapters.converter.exception_converter import (
    ExceptionConverter,
    parse_sandbox_error,
)
from opensandbox.adapters.converter.execution_converter import (
    ExecutionConverter,
)
from opensandbox.adapters.converter.filesystem_model_converter import (
    FilesystemModelConverter,
)
from opensandbox.adapters.converter.metrics_model_converter import (
    MetricsModelConverter,
)
from opensandbox.adapters.converter.response_handler import (
    handle_api_error,
    require_parsed,
)
from opensandbox.adapters.converter.sandbox_model_converter import (
    SandboxModelConverter,
)
from opensandbox.api.lifecycle.errors import UnexpectedStatus
from opensandbox.exceptions import (
    InvalidArgumentException,
    SandboxApiException,
    SandboxInternalException,
)
from opensandbox.models.execd import RunCommandOpts
from opensandbox.models.sandboxes import (
    NetworkPolicy,
    NetworkRule,
    PlatformSpec,
    SandboxImageSpec,
)


def test_parse_sandbox_error_from_json_bytes() -> None:
    err = parse_sandbox_error(b'{"code":"X","message":"m"}')
    assert err is not None
    assert err.code == "X"
    assert err.message == "m"


def test_parse_sandbox_error_from_plain_text_string() -> None:
    err = parse_sandbox_error("not-json")
    assert err is not None
    assert err.code == "UNEXPECTED_RESPONSE"
    assert err.message == "not-json"


def test_parse_sandbox_error_from_invalid_utf8_bytes_fallback_message() -> None:
    err = parse_sandbox_error(b"\xff\xfe")
    assert err is not None
    assert err.code == "UNEXPECTED_RESPONSE"
    assert err.message is not None
    assert "\ufffd" in err.message


def test_handle_api_error_raises_with_parsed_message() -> None:
    class Parsed:
        code = "BAD_REQUEST"
        message = "bad request"

    class Resp:
        status_code = 400
        parsed = Parsed()
        headers = {"X-Request-ID": "req-123"}

    with pytest.raises(SandboxApiException) as ei:
        handle_api_error(Resp(), "Op")
    assert "bad request" in str(ei.value)
    assert ei.value.request_id == "req-123"
    assert ei.value.error.code == "BAD_REQUEST"
    assert ei.value.error.message == "bad request"


def test_handle_api_error_noop_on_success() -> None:
    class Resp:
        status_code = 200
        parsed = None

    handle_api_error(Resp(), "Op")


def test_require_parsed_includes_request_id_on_invalid_payload() -> None:
    class Resp:
        status_code = 200
        parsed = None
        headers = {"x-request-id": "req-456"}

    with pytest.raises(SandboxApiException) as ei:
        require_parsed(Resp(), expected_type=str, operation_name="Op")
    assert ei.value.request_id == "req-456"


def test_exception_converter_maps_common_types() -> None:
    se = ExceptionConverter.to_sandbox_exception(ValueError("x"))
    assert isinstance(se, InvalidArgumentException)

    se2 = ExceptionConverter.to_sandbox_exception(OSError("x"))
    assert isinstance(se2, SandboxInternalException)


def test_exception_converter_maps_generated_unexpected_status_to_api_exception() -> (
    None
):
    err = UnexpectedStatus(400, b'{"code":"X","message":"bad"}')

    converted = ExceptionConverter.to_sandbox_exception(err)

    assert isinstance(converted, SandboxApiException)
    assert converted.status_code == 400
    assert converted.error is not None
    assert converted.error.code == "X"


def test_exception_converter_maps_httpx_status_error_to_api_exception() -> None:
    request = Request("GET", "https://example.test")
    response = Response(
        502, request=request, content=b'{"code":"UPSTREAM","message":"gateway"}'
    )
    err = HTTPStatusError("bad gateway", request=request, response=response)

    converted = ExceptionConverter.to_sandbox_exception(err)

    assert isinstance(converted, SandboxApiException)
    assert converted.status_code == 502
    assert converted.error is not None
    assert converted.error.code == "UPSTREAM"


def test_execution_converter_to_api_run_command_request() -> None:
    from opensandbox.api.execd.types import UNSET

    api = ExecutionConverter.to_api_run_command_request("echo hi", RunCommandOpts())
    d = api.to_dict()
    assert d["command"] == "echo hi"
    assert "cwd" not in d

    api2 = ExecutionConverter.to_api_run_command_request(
        "echo hi",
        RunCommandOpts(working_directory="/tmp"),
    )
    d2 = api2.to_dict()
    assert d2["cwd"] == "/tmp"
    # background defaults to False in domain opts; when False we omit it from the API request.
    assert d2.get("background", UNSET) is UNSET

    from datetime import timedelta

    api3 = ExecutionConverter.to_api_run_command_request(
        "sleep 10",
        RunCommandOpts(timeout=timedelta(seconds=60)),
    )
    d3 = api3.to_dict()
    assert d3["command"] == "sleep 10"
    assert d3["timeout"] == 60_000
    # timeout omitted when not set (backward compat)
    assert (
        "timeout"
        not in ExecutionConverter.to_api_run_command_request(
            "x", RunCommandOpts()
        ).to_dict()
    )

    api4 = ExecutionConverter.to_api_run_command_request(
        "id",
        RunCommandOpts(
            uid=1000,
            gid=1000,
            envs={"APP_ENV": "test", "LOG_LEVEL": "debug"},
        ),
    )
    d4 = api4.to_dict()
    assert d4["uid"] == 1000
    assert d4["gid"] == 1000
    assert d4["envs"] == {"APP_ENV": "test", "LOG_LEVEL": "debug"}
    assert "cwd" not in d4


def test_run_command_opts_validates_gid_requires_uid() -> None:
    with pytest.raises(ValueError, match="uid is required when gid is provided"):
        RunCommandOpts(gid=1000)


def test_filesystem_and_metrics_converters() -> None:
    from datetime import datetime, timezone

    from opensandbox.api.execd.models import FileInfo, Metrics

    fi = FileInfo(
        path="/a",
        mode=644,
        owner="u",
        group="g",
        size=1,
        modified_at=datetime(2025, 1, 1, tzinfo=timezone.utc),
        created_at=datetime(2025, 1, 1, tzinfo=timezone.utc),
    )
    entry = FilesystemModelConverter.to_entry_info(fi)
    assert entry.path == "/a"

    api_metrics = Metrics(
        cpu_count=1.0,
        cpu_used_pct=2.0,
        mem_total_mib=3.0,
        mem_used_mib=4.0,
        timestamp=5,
    )
    m = MetricsModelConverter.to_sandbox_metrics(api_metrics)
    assert m.cpu_used_percentage == 2.0


def test_sandbox_model_converter_to_api_create_request_and_renew_tz() -> None:
    from datetime import timezone

    spec = SandboxImageSpec("python:3.11")
    req = SandboxModelConverter.to_api_create_sandbox_request(
        spec=spec,
        entrypoint=["/bin/sh"],
        env={},
        metadata={},
        timeout=timedelta(seconds=3),
        resource={"cpu": "100m"},
        platform=PlatformSpec(os="linux", arch="arm64"),
        network_policy=NetworkPolicy(
            defaultAction="deny",
            egress=[NetworkRule(action="allow", target="pypi.org")],
        ),
        extensions={},
        volumes=None,
    )
    d = req.to_dict()
    assert d["image"]["uri"] == "python:3.11"
    assert d["timeout"] == 3
    assert "env" not in d
    assert "metadata" not in d
    assert d["platform"] == {"os": "linux", "arch": "arm64"}
    assert d["networkPolicy"]["defaultAction"] == "deny"
    assert d["networkPolicy"]["egress"] == [{"action": "allow", "target": "pypi.org"}]

    renew = SandboxModelConverter.to_api_renew_request(datetime(2025, 1, 1))
    assert renew.expires_at.tzinfo is timezone.utc


def test_platform_spec_accepts_windows() -> None:
    platform = PlatformSpec(os="windows", arch="amd64")
    assert platform.os == "windows"
    assert platform.arch == "amd64"


def test_sandbox_model_converter_preserves_null_timeout_for_manual_cleanup() -> None:
    req = SandboxModelConverter.to_api_create_sandbox_request(
        spec=SandboxImageSpec("python:3.11"),
        entrypoint=["/bin/sh"],
        env={},
        metadata={},
        timeout=None,
        resource={"cpu": "100m"},
        platform=None,
        network_policy=None,
        extensions={},
        volumes=None,
    )

    dumped = req.to_dict()
    assert dumped["timeout"] is None


def test_sandbox_model_converter_snapshot_restore_request() -> None:
    req = SandboxModelConverter.to_api_create_sandbox_request(
        spec=None,
        entrypoint=None,
        env={},
        metadata={},
        timeout=None,
        resource={"cpu": "100m"},
        platform=None,
        network_policy=None,
        extensions={},
        volumes=None,
        snapshot_id="snap-123",
    )

    dumped = req.to_dict()
    assert dumped["snapshotId"] == "snap-123"
    assert "image" not in dumped
    assert "entrypoint" not in dumped


def test_sandbox_model_converter_maps_platform_from_create_response() -> None:
    from opensandbox.api.lifecycle.models.create_sandbox_response import (
        CreateSandboxResponse,
    )
    from opensandbox.api.lifecycle.models.platform_spec import (
        PlatformSpec as ApiPlatformSpec,
    )
    from opensandbox.api.lifecycle.models.sandbox_status import SandboxStatus

    api_response = CreateSandboxResponse(
        id="sbx-1",
        status=SandboxStatus(state="Running"),
        platform=ApiPlatformSpec(os="linux", arch="arm64"),
        created_at=datetime(2025, 1, 1),
        entrypoint=["/bin/sh"],
    )

    converted = SandboxModelConverter.to_sandbox_create_response(api_response)
    assert converted.platform is not None
    assert converted.platform.arch == "arm64"


def test_sandbox_model_converter_supports_windows_platform_request() -> None:
    req = SandboxModelConverter.to_api_create_sandbox_request(
        spec=SandboxImageSpec("dockurr/windows:latest"),
        entrypoint=["cmd", "/c", "echo hi"],
        env={},
        metadata={},
        timeout=timedelta(seconds=3),
        resource={"cpu": "2", "memory": "4G"},
        platform=PlatformSpec(os="windows", arch="amd64"),
        network_policy=None,
        extensions={},
        volumes=None,
    )
    dumped = req.to_dict()
    assert dumped["platform"] == {"os": "windows", "arch": "amd64"}
