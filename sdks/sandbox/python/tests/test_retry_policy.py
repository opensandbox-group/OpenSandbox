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
"""Unit tests for RetryPolicy and related retry configuration types."""

from __future__ import annotations

from datetime import timedelta
from http import HTTPStatus

import pytest

from opensandbox.transport import JitterMode, RetryCause, RetryPolicy


class TestRetryPolicyDefaults:
    def test_default_policy_enables_retry_for_idempotent(self) -> None:
        p = RetryPolicy()
        assert p.max_retries == 3
        assert p.retryable_statuses_for("GET") == frozenset(
            {
                HTTPStatus.TOO_MANY_REQUESTS,
                HTTPStatus.BAD_GATEWAY,
                HTTPStatus.SERVICE_UNAVAILABLE,
            }
        )

    def test_default_policy_disables_post_status_retry(self) -> None:
        p = RetryPolicy()
        assert p.retryable_statuses_for("POST") == frozenset()
        assert p.retryable_statuses_for("PATCH") == frozenset()

    def test_disabled_has_zero_retries(self) -> None:
        p = RetryPolicy.disabled()
        assert p.max_retries == 0

    def test_default_jitter_is_decorrelated(self) -> None:
        assert RetryPolicy().jitter is JitterMode.DECORRELATED

    def test_opt_in_post_status_set(self) -> None:
        p = RetryPolicy(
            retryable_status_codes_non_idempotent=frozenset(
                {HTTPStatus.TOO_MANY_REQUESTS, HTTPStatus.BAD_GATEWAY}
            ),
        )
        assert p.retryable_statuses_for("POST") == frozenset(
            {HTTPStatus.TOO_MANY_REQUESTS, HTTPStatus.BAD_GATEWAY}
        )
        # Idempotent set unchanged.
        assert HTTPStatus.BAD_GATEWAY in p.retryable_statuses_for("GET")


class TestRetryPolicyValidation:
    def test_negative_max_retries_rejected(self) -> None:
        with pytest.raises(ValueError):
            RetryPolicy(max_retries=-1)

    def test_negative_initial_backoff_rejected(self) -> None:
        with pytest.raises(ValueError):
            RetryPolicy(initial_backoff=timedelta(seconds=-1))

    def test_multiplier_below_one_rejected(self) -> None:
        with pytest.raises(ValueError):
            RetryPolicy(backoff_multiplier=0.5)

    def test_non_positive_per_attempt_timeout_rejected(self) -> None:
        with pytest.raises(ValueError):
            RetryPolicy(per_attempt_timeout=timedelta(seconds=-1))
        with pytest.raises(ValueError):
            RetryPolicy(per_attempt_timeout=timedelta(0))

    def test_non_positive_overall_deadline_rejected(self) -> None:
        with pytest.raises(ValueError):
            RetryPolicy(overall_deadline=timedelta(seconds=-1))
        with pytest.raises(ValueError):
            RetryPolicy(overall_deadline=timedelta(0))


class TestRetryCause:
    @pytest.mark.parametrize(
        "code,expected",
        [
            (408, RetryCause.STATUS_408),
            (425, RetryCause.STATUS_425),
            (429, RetryCause.STATUS_429),
            (500, RetryCause.STATUS_500),
            (502, RetryCause.STATUS_502),
            (503, RetryCause.STATUS_503),
            (504, RetryCause.STATUS_504),
            (200, RetryCause.STATUS_OTHER),
            (418, RetryCause.STATUS_OTHER),
            (501, RetryCause.STATUS_OTHER),
        ],
    )
    def test_for_status(self, code: int, expected: RetryCause) -> None:
        assert RetryCause.for_status(code) is expected
