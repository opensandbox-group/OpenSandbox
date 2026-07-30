#
# Copyright 2026 Alibaba Group Holding Ltd.
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
"""Extended exception hierarchy and the ``is_retryable`` accessor."""

from __future__ import annotations

from datetime import timedelta

from opensandbox.exceptions import (
    SandboxApiException,
    SandboxConnectionException,
    SandboxException,
    SandboxInternalException,
    SandboxRateLimitException,
    SandboxTimeoutException,
)


class TestHierarchy:
    def test_rate_limit_inherits_api(self) -> None:
        exc = SandboxRateLimitException(
            message="throttled", retry_after=timedelta(seconds=2.5)
        )
        assert isinstance(exc, SandboxApiException)
        assert isinstance(exc, SandboxException)
        assert exc.retry_after == timedelta(seconds=2.5)
        assert exc.status_code == 429

    def test_timeout_inherits_internal(self) -> None:
        exc = SandboxTimeoutException(message="overall_deadline exceeded")
        assert isinstance(exc, SandboxInternalException)
        assert isinstance(exc, SandboxException)

    def test_connection_inherits_internal(self) -> None:
        exc = SandboxConnectionException(message="dns failed")
        assert isinstance(exc, SandboxInternalException)
        assert isinstance(exc, SandboxException)


class TestIsRetryable:
    def test_default_false(self) -> None:
        assert SandboxException("boom").is_retryable is False
        assert SandboxApiException("boom", status_code=500).is_retryable is False
        assert SandboxInternalException("boom").is_retryable is False

    def test_flag_flows_through_subclasses(self) -> None:
        assert (
            SandboxRateLimitException(is_retryable=True).is_retryable is True
        )
        assert (
            SandboxTimeoutException(is_retryable=True).is_retryable is True
        )
        assert (
            SandboxConnectionException(is_retryable=True).is_retryable is True
        )

    def test_existing_call_sites_backward_compatible(self) -> None:
        """SandboxApiException still accepts its original kwargs."""
        exc = SandboxApiException(
            message="oops",
            status_code=500,
            request_id="req-123",
        )
        assert exc.status_code == 500
        assert exc.request_id == "req-123"
        assert exc.is_retryable is False
