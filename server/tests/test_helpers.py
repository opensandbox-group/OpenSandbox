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

from datetime import datetime, timedelta, timezone

from opensandbox_server.api.schema import ImageSpec, Sandbox, SandboxStatus
from opensandbox_server.services.helpers import parse_timestamp


def minimal_sandbox(sandbox_id: str = "sbx-001") -> Sandbox:
    """Minimal valid Sandbox for stubbing ``get_sandbox`` in route tests."""
    now = datetime.now(timezone.utc)
    return Sandbox(
        id=sandbox_id,
        image=ImageSpec(uri="test:latest"),
        status=SandboxStatus(state="Running"),
        metadata={},
        entrypoint=["sh"],
        expiresAt=now + timedelta(hours=1),
        createdAt=now,
    )


def test_parse_timestamp_truncates_nanoseconds():
    ts = "2025-12-10T05:29:56.359015208Z"

    result = parse_timestamp(ts)

    assert result.tzinfo is not None
    assert result.astimezone(timezone.utc) == result
    assert result.year == 2025
    assert result.month == 12
    assert result.day == 10
    assert result.microsecond == 359015


def test_parse_timestamp_parses_valid_rfc3339():
    ts = "2024-01-01T12:34:56.123456Z"

    result = parse_timestamp(ts)

    assert result.tzinfo is not None
    assert result == datetime(2024, 1, 1, 12, 34, 56, 123456, tzinfo=timezone.utc)


def test_parse_timestamp_invalid_falls_back_to_now():
    before = datetime.now(timezone.utc)
    result = parse_timestamp("not-a-time")
    after = datetime.now(timezone.utc)

    assert result.tzinfo is not None
    assert before <= result <= after
