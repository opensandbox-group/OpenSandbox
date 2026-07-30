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
"""Unit tests for the pure retry decision function and backoff helpers."""

from __future__ import annotations

import random
from datetime import datetime, timedelta, timezone
from http import HTTPStatus

import pytest

from opensandbox.transport import JitterMode, RetryPolicy
from opensandbox.transport._decision import (
    Outcome,
    apply_retry_after_cap,
    compute_backoff,
    parse_retry_after,
    should_retry,
)


def _status_outcome(code: int) -> Outcome:
    return Outcome(is_transport_error=False, status_code=code)


class TestDecisionBudgetAndDeadline:
    def test_budget_exhausted_no_retry(self) -> None:
        p = RetryPolicy(max_retries=3)
        assert not should_retry(
            "GET", _status_outcome(503), retries_used=3, policy=p, elapsed=timedelta(0)
        )

    def test_overall_deadline_expired_no_retry(self) -> None:
        p = RetryPolicy(overall_deadline=timedelta(seconds=1))
        assert not should_retry(
            "GET",
            _status_outcome(503),
            retries_used=0,
            policy=p,
            elapsed=timedelta(seconds=2),
        )

    def test_cancellation_terminal(self) -> None:
        p = RetryPolicy()
        assert not should_retry(
            "GET",
            _status_outcome(503),
            retries_used=0,
            policy=p,
            elapsed=timedelta(0),
            cancelled=True,
        )


class TestDecisionStatusMatrix:
    """Table-driven test mirroring the documented decision matrix."""

    @pytest.mark.parametrize(
        "method,status,expected",
        [
            # Idempotent, default retryable set.
            ("GET", 429, True),
            ("GET", 502, True),
            ("GET", 503, True),
            ("HEAD", 502, True),
            ("PUT", 502, True),
            ("DELETE", 503, True),
            # Idempotent, non-retryable statuses.
            ("GET", 400, False),
            ("GET", 401, False),
            ("GET", 403, False),
            ("GET", 404, False),
            ("GET", 408, False),
            ("GET", 409, False),
            ("GET", 425, False),
            ("GET", 500, False),
            ("GET", 501, False),
            ("GET", 504, False),
            # Non-idempotent, default: never retry on status.
            ("POST", 429, False),
            ("POST", 500, False),
            ("POST", 502, False),
            ("POST", 503, False),
            ("POST", 504, False),
            ("PATCH", 502, False),
        ],
    )
    def test_status_matrix_default(
        self, method: str, status: int, expected: bool
    ) -> None:
        p = RetryPolicy()
        assert (
            should_retry(
                method,
                _status_outcome(status),
                retries_used=0,
                policy=p,
                elapsed=timedelta(0),
            )
            is expected
        )

    def test_post_opt_in_lifts_selected_statuses(self) -> None:
        p = RetryPolicy(
            retryable_status_codes_non_idempotent=frozenset(
                {HTTPStatus.TOO_MANY_REQUESTS, HTTPStatus.BAD_GATEWAY}
            )
        )
        assert should_retry(
            "POST", _status_outcome(429), 0, p, timedelta(0)
        )
        assert should_retry(
            "POST", _status_outcome(502), 0, p, timedelta(0)
        )
        # Not in caller's set.
        assert not should_retry(
            "POST", _status_outcome(503), 0, p, timedelta(0)
        )
        assert not should_retry(
            "POST", _status_outcome(504), 0, p, timedelta(0)
        )


class TestDecisionTransportBranch:
    def test_pre_send_retries_on_any_method(self) -> None:
        p = RetryPolicy()
        pre_send = Outcome(is_transport_error=True, is_pre_send=True)
        assert should_retry("GET", pre_send, 0, p, timedelta(0))
        assert should_retry("POST", pre_send, 0, p, timedelta(0))
        assert should_retry("PATCH", pre_send, 0, p, timedelta(0))

    def test_post_send_only_idempotent(self) -> None:
        p = RetryPolicy()
        post_send = Outcome(is_transport_error=True, is_pre_send=False)
        assert should_retry("GET", post_send, 0, p, timedelta(0))
        assert not should_retry("POST", post_send, 0, p, timedelta(0))
        assert not should_retry("PATCH", post_send, 0, p, timedelta(0))

    def test_opaque_transport_only_idempotent(self) -> None:
        p = RetryPolicy()
        opaque = Outcome(
            is_transport_error=True, is_pre_send=False, is_opaque_transport=True
        )
        assert should_retry("GET", opaque, 0, p, timedelta(0))
        assert not should_retry("POST", opaque, 0, p, timedelta(0))


class TestDecisionNonReplayableBody:
    def test_status_retry_suppressed_for_non_replayable_body(self) -> None:
        # POST opted into 503 status retry, but a non-replayable body
        # must never be resent.
        p = RetryPolicy(
            retryable_status_codes_non_idempotent=frozenset(
                {HTTPStatus.SERVICE_UNAVAILABLE}
            )
        )
        out = _status_outcome(503)
        assert should_retry(
            "POST", out, 0, p, timedelta(0), body_replayable=True
        )
        assert not should_retry(
            "POST", out, 0, p, timedelta(0), body_replayable=False
        )

    def test_pre_send_first_attempt_allowed_then_suppressed(self) -> None:
        # A non-replayable body may still be retried once on a pre-send
        # failure (no bytes written yet), but not after that.
        p = RetryPolicy()
        pre_send = Outcome(is_transport_error=True, is_pre_send=True)
        assert should_retry(
            "POST", pre_send, 0, p, timedelta(0), body_replayable=False
        )
        assert not should_retry(
            "POST", pre_send, 1, p, timedelta(0), body_replayable=False
        )

    def test_replayable_body_unaffected(self) -> None:
        # Replayable bodies keep the normal pre-send behavior even past
        # the first attempt.
        p = RetryPolicy(max_retries=10)
        pre_send = Outcome(is_transport_error=True, is_pre_send=True)
        assert should_retry(
            "POST", pre_send, 3, p, timedelta(0), body_replayable=True
        )

    def test_post_send_never_lifted_by_status_opt_in(self) -> None:
        """Status-code opt-in must not lift the post-send transport rule."""
        p = RetryPolicy(
            retryable_status_codes_non_idempotent=frozenset(
                {HTTPStatus.TOO_MANY_REQUESTS, HTTPStatus.BAD_GATEWAY}
            )
        )
        post_send = Outcome(is_transport_error=True, is_pre_send=False)
        assert not should_retry("POST", post_send, 0, p, timedelta(0))


class TestBackoff:
    def test_none_jitter_deterministic_exponential(self) -> None:
        p = RetryPolicy(
            jitter=JitterMode.NONE,
            initial_backoff=timedelta(milliseconds=100),
            max_backoff=timedelta(seconds=10),
            backoff_multiplier=2.0,
        )
        r = random.Random(0)
        assert compute_backoff(0, p, timedelta(0), r) == timedelta(milliseconds=100)
        assert compute_backoff(1, p, timedelta(0), r) == timedelta(milliseconds=200)
        assert compute_backoff(2, p, timedelta(0), r) == timedelta(milliseconds=400)

    def test_none_jitter_capped_by_max_backoff(self) -> None:
        p = RetryPolicy(
            jitter=JitterMode.NONE,
            initial_backoff=timedelta(seconds=1),
            max_backoff=timedelta(seconds=3),
        )
        r = random.Random(0)
        assert compute_backoff(10, p, timedelta(0), r) == timedelta(seconds=3)

    def test_full_jitter_bounded(self) -> None:
        p = RetryPolicy(
            jitter=JitterMode.FULL,
            initial_backoff=timedelta(milliseconds=100),
            max_backoff=timedelta(seconds=1),
        )
        r = random.Random(42)
        for retry_index in range(5):
            sleep = compute_backoff(retry_index, p, timedelta(0), r)
            assert timedelta(0) <= sleep <= timedelta(seconds=1)

    def test_decorrelated_jitter_grows_but_bounded(self) -> None:
        p = RetryPolicy(
            jitter=JitterMode.DECORRELATED,
            initial_backoff=timedelta(milliseconds=100),
            max_backoff=timedelta(seconds=5),
            backoff_multiplier=2.0,
        )
        r = random.Random(1234)
        previous = timedelta(0)
        for retry_index in range(10):
            sleep = compute_backoff(retry_index, p, previous, r)
            assert sleep >= timedelta(0)
            assert sleep <= p.max_backoff
            previous = sleep

    def test_decorrelated_first_delay_respects_max_backoff(self) -> None:
        # max_backoff < initial_backoff must clamp the first retry too.
        p = RetryPolicy(
            jitter=JitterMode.DECORRELATED,
            initial_backoff=timedelta(seconds=1),
            max_backoff=timedelta(milliseconds=50),
        )
        r = random.Random(0)
        assert compute_backoff(0, p, timedelta(0), r) == timedelta(milliseconds=50)

    def test_decorrelated_first_delay_zero_max_backoff(self) -> None:
        # ``max_backoff=0`` means "retry immediately"; the first delay
        # must respect it despite ``initial_backoff`` being non-zero.
        p = RetryPolicy(
            jitter=JitterMode.DECORRELATED,
            initial_backoff=timedelta(seconds=1),
            max_backoff=timedelta(0),
        )
        r = random.Random(0)
        assert compute_backoff(0, p, timedelta(0), r) == timedelta(0)


class TestRetryAfter:
    def test_parse_delta_seconds(self) -> None:
        assert parse_retry_after("5") == timedelta(seconds=5)
        assert parse_retry_after("0") == timedelta(0)
        assert parse_retry_after("-3") == timedelta(0)

    def test_parse_http_date(self) -> None:
        now = datetime(2026, 7, 22, 12, 0, 0, tzinfo=timezone.utc)
        future = "Wed, 22 Jul 2026 12:00:30 GMT"
        parsed = parse_retry_after(future, now=now)
        assert parsed is not None
        assert timedelta(seconds=29) <= parsed <= timedelta(seconds=31)

    def test_parse_http_date_in_past_normalized_to_zero(self) -> None:
        now = datetime(2026, 7, 22, 12, 0, 0, tzinfo=timezone.utc)
        past = "Wed, 22 Jul 2026 11:59:00 GMT"
        assert parse_retry_after(past, now=now) == timedelta(0)

    def test_parse_unparseable(self) -> None:
        assert parse_retry_after("garbage") is None
        assert parse_retry_after(None) is None
        assert parse_retry_after("") is None

    def test_parse_huge_numeric_does_not_overflow(self) -> None:
        # A pathologically large numeric Retry-After must not raise
        # OverflowError from timedelta construction; it is clamped up
        # front to a safe upper bound. The 60s operational ceiling is
        # applied separately by apply_retry_after_cap.
        huge = "999999999999999999999999"
        parsed = parse_retry_after(huge)
        assert parsed is not None
        assert parsed.total_seconds() > 0
        assert apply_retry_after_cap(parsed) == timedelta(seconds=60)

    def test_cap_applied(self) -> None:
        # Values above the fixed 60s ceiling are clamped; smaller values
        # pass through unchanged; None stays None.
        assert (
            apply_retry_after_cap(timedelta(seconds=3600))
            == timedelta(seconds=60)
        )
        assert (
            apply_retry_after_cap(timedelta(seconds=10))
            == timedelta(seconds=10)
        )
        assert apply_retry_after_cap(None) is None
