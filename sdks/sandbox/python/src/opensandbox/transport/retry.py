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
"""Retry policy, event, and cause enums. No httpx dependency."""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import timedelta
from enum import Enum
from http import HTTPStatus

# RFC 9110 §9.2.2.
_IDEMPOTENT_METHODS: frozenset[str] = frozenset(
    {"GET", "HEAD", "PUT", "DELETE", "OPTIONS"}
)


def is_idempotent_method(method: str) -> bool:
    return method.upper() in _IDEMPOTENT_METHODS


class JitterMode(str, Enum):
    """Backoff jitter algorithm."""

    NONE = "none"
    FULL = "full"
    DECORRELATED = "decorrelated"


class RetryCause(str, Enum):
    """Failure classification carried on :class:`RetryEvent`."""

    PRE_SEND = "pre_send"
    OPAQUE_TRANSPORT = "opaque_transport"
    READ_TIMEOUT = "read_timeout"
    WRITE_TIMEOUT = "write_timeout"
    UNEXPECTED_EOF = "unexpected_eof"
    STATUS_408 = "status_408"
    STATUS_425 = "status_425"
    STATUS_429 = "status_429"
    STATUS_500 = "status_500"
    STATUS_502 = "status_502"
    STATUS_503 = "status_503"
    STATUS_504 = "status_504"
    STATUS_OTHER = "status_other"

    @classmethod
    def for_status(cls, status_code: int) -> RetryCause:
        try:
            status = HTTPStatus(status_code)
        except ValueError:
            return cls.STATUS_OTHER
        return {
            HTTPStatus.REQUEST_TIMEOUT: cls.STATUS_408,
            HTTPStatus.TOO_EARLY: cls.STATUS_425,
            HTTPStatus.TOO_MANY_REQUESTS: cls.STATUS_429,
            HTTPStatus.INTERNAL_SERVER_ERROR: cls.STATUS_500,
            HTTPStatus.BAD_GATEWAY: cls.STATUS_502,
            HTTPStatus.SERVICE_UNAVAILABLE: cls.STATUS_503,
            HTTPStatus.GATEWAY_TIMEOUT: cls.STATUS_504,
        }.get(status, cls.STATUS_OTHER)


@dataclass(frozen=True)
class RetryEvent:
    """Payload passed to :attr:`RetryPolicy.on_retry` before each backoff sleep."""

    attempt: int
    """1-based index of the upcoming attempt (retry #1 is ``attempt=2``)."""

    retries_used: int
    """Retries consumed so far, i.e. ``attempt - 1``."""

    method: str
    url: str
    cause: RetryCause
    status_code: int | None
    backoff: timedelta
    request_id: str | None
    """``X-Request-ID`` header from the last response, if present."""

    exception: BaseException | None


_DEFAULT_IDEMPOTENT_STATUS: frozenset[HTTPStatus] = frozenset(
    {
        HTTPStatus.TOO_MANY_REQUESTS,   # 429
        HTTPStatus.BAD_GATEWAY,         # 502
        HTTPStatus.SERVICE_UNAVAILABLE, # 503
    }
)


@dataclass(frozen=True)
class RetryPolicy:
    """
    Retry configuration.

    Defaults retry idempotent methods only. Use :meth:`disabled` for
    fast-fail. Extend ``retryable_status_codes_non_idempotent`` to opt
    ``POST``/``PATCH`` in on specific status codes; the SDK never does
    this on the caller's behalf.
    """

    max_retries: int = 3
    """Retries after the initial attempt. ``0`` disables retry."""

    initial_backoff: timedelta = timedelta(milliseconds=500)
    max_backoff: timedelta = timedelta(seconds=30)
    backoff_multiplier: float = 2.0
    jitter: JitterMode = JitterMode.DECORRELATED

    retryable_status_codes_idempotent: frozenset[HTTPStatus] = _DEFAULT_IDEMPOTENT_STATUS
    """Statuses that trigger retry for ``GET/HEAD/PUT/DELETE/OPTIONS``."""

    retryable_status_codes_non_idempotent: frozenset[HTTPStatus] = field(
        default_factory=frozenset
    )
    """Statuses that trigger retry for ``POST/PATCH``. Empty by default."""

    per_attempt_timeout: timedelta | None = None
    overall_deadline: timedelta | None = None
    """Wall-clock cap across all attempts of one logical request."""

    on_retry: Callable[[RetryEvent], None] | None = None
    """Non-blocking hook fired synchronously before each backoff sleep."""

    @classmethod
    def disabled(cls) -> RetryPolicy:
        """Never retry. Also suppresses fresh-connection recovery."""
        return cls(max_retries=0)

    def wraps_transport(self) -> bool:
        """
        Whether this policy requires the retry transport to be installed.

        Even ``max_retries == 0`` needs the wrapper when the caller uses
        it purely to enforce ``per_attempt_timeout`` or
        ``overall_deadline`` on a single attempt.
        """
        return (
            self.max_retries > 0
            or self.per_attempt_timeout is not None
            or self.overall_deadline is not None
            or self.on_retry is not None
        )

    def retryable_statuses_for(self, method: str) -> frozenset[HTTPStatus]:
        if is_idempotent_method(method):
            return frozenset(self.retryable_status_codes_idempotent)
        return frozenset(self.retryable_status_codes_non_idempotent)

    def __post_init__(self) -> None:
        if self.max_retries < 0:
            raise ValueError(
                f"max_retries must be >= 0, got {self.max_retries!r}"
            )
        if self.initial_backoff.total_seconds() < 0:
            raise ValueError(
                f"initial_backoff must be >= 0, got {self.initial_backoff!r}"
            )
        if self.max_backoff.total_seconds() < 0:
            raise ValueError(
                f"max_backoff must be >= 0, got {self.max_backoff!r}"
            )
        if self.backoff_multiplier < 1.0:
            raise ValueError(
                f"backoff_multiplier must be >= 1.0, got {self.backoff_multiplier!r}"
            )
        if (
            self.per_attempt_timeout is not None
            and self.per_attempt_timeout.total_seconds() <= 0
        ):
            raise ValueError(
                f"per_attempt_timeout must be > 0 when set, got {self.per_attempt_timeout!r}"
            )
        if (
            self.overall_deadline is not None
            and self.overall_deadline.total_seconds() <= 0
        ):
            raise ValueError(
                f"overall_deadline must be > 0 when set, got {self.overall_deadline!r}"
            )
        # Normalize so callers can pass a plain ``set`` or ``tuple``.
        object.__setattr__(
            self,
            "retryable_status_codes_idempotent",
            frozenset(self.retryable_status_codes_idempotent),
        )
        object.__setattr__(
            self,
            "retryable_status_codes_non_idempotent",
            frozenset(self.retryable_status_codes_non_idempotent),
        )
