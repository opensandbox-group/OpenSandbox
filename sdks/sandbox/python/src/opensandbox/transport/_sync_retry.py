#
# Copyright 2026 The OpenSandbox Authors
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
"""Sync httpx transport wrapper enforcing :class:`RetryPolicy`."""

from __future__ import annotations

import logging
import random
import time
from datetime import timedelta

import httpx

from opensandbox.exceptions import SandboxWakeTimeoutException
from opensandbox.transport._async_retry import (
    _capture_baseline_timeout,
    _clamp_attempt_timeout,
)
from opensandbox.transport._classify import (
    classify_transport_exception,
    is_body_replayable,
    is_wake_timeout_response,
    outcome_for_response,
)
from opensandbox.transport._deadline_sync import DEADLINE_EXTENSION
from opensandbox.transport._decision import (
    Outcome,
    apply_retry_after_cap,
    compute_backoff,
    parse_retry_after,
    should_retry,
    should_retry_wake,
)
from opensandbox.transport.retry import (
    WAKE_DEFAULT_RETRY_AFTER,
    RetryCause,
    RetryEvent,
    RetryPolicy,
)

logger = logging.getLogger(__name__)


class RetrySyncTransport(httpx.BaseTransport):
    """Sync counterpart of :class:`RetryAsyncTransport`."""

    def __init__(
        self,
        inner: httpx.BaseTransport,
        policy: RetryPolicy,
        *,
        owns_inner: bool = False,
        rng: random.Random | None = None,
    ) -> None:
        self._inner = inner
        self._policy = policy
        self._owns_inner = owns_inner
        self._rng = rng or random.Random()

    @property
    def inner(self) -> httpx.BaseTransport:
        """The wrapped raw transport."""
        return self._inner

    def for_streaming(self) -> httpx.BaseTransport:
        """Transport for streaming (SSE) clients; see :meth:`RetryPolicy.wake_only`."""
        if not self._policy.wake_retry_enabled:
            return self._inner
        return RetrySyncTransport(
            self._inner,
            self._policy.wake_only(),
            owns_inner=self._owns_inner,
            rng=self._rng,
        )

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        policy = self._policy
        method = request.method.upper()
        start = time.monotonic()
        previous_sleep = timedelta(0)
        retries_used = 0
        wake_attempts_used = 0
        last_exc: BaseException | None = None
        body_replayable = is_body_replayable(request)

        baseline_timeout = _capture_baseline_timeout(request)

        while True:
            elapsed = timedelta(seconds=time.monotonic() - start)
            remaining = (
                policy.overall_deadline - elapsed
                if policy.overall_deadline is not None
                else None
            )
            readiness_deadline = request.extensions.get(DEADLINE_EXTENSION)
            if readiness_deadline is not None:
                readiness_remaining = timedelta(seconds=readiness_deadline - time.monotonic())
                remaining = min(remaining, readiness_remaining) if remaining is not None else readiness_remaining
            if remaining is not None and remaining.total_seconds() <= 0:
                timeout_exc = httpx.ReadTimeout(
                    "retry overall_deadline exceeded before next attempt"
                )
                if last_exc is not None:
                    timeout_exc.__cause__ = last_exc
                raise timeout_exc
            _clamp_attempt_timeout(request, policy, baseline_timeout, remaining)

            outcome: Outcome
            exc: BaseException | None = None
            response: httpx.Response | None = None
            try:
                response = self._inner.handle_request(request)
                outcome = outcome_for_response(response)
            except httpx.TransportError as e:
                exc = e
                outcome = classify_transport_exception(e)

            elapsed = timedelta(seconds=time.monotonic() - start)

            # OSEP-0024 wake branch. The marker proves the gateway parked
            # the request without forwarding it, so the original request
            # may be resent for any method (replayable body) — governed
            # by the wake budget, not the general retry budget.
            if (
                response is not None
                and policy.wake_retry_enabled
                and body_replayable
                and is_wake_timeout_response(response)
            ):
                wake_attempts_used += 1
                # Cap before falling back: the same pathological-header
                # ceiling as the general path; without it a marked 503
                # carrying Retry-After: 86400 would park this attempt
                # for a day in a single sleep.
                retry_after = apply_retry_after_cap(
                    parse_retry_after(response.headers.get("Retry-After"))
                )
                if retry_after is None:
                    retry_after = WAKE_DEFAULT_RETRY_AFTER
                if not should_retry_wake(
                    wake_attempts_used=wake_attempts_used,
                    policy=policy,
                    elapsed=elapsed,
                    retry_after=retry_after,
                ):
                    body: bytes | None = None
                    try:
                        body = response.read()
                    except Exception:
                        logger.debug(
                            "failed to read wake-timeout body", exc_info=True
                        )
                    request_id = response.headers.get("X-Request-ID")
                    try:
                        response.close()
                    except Exception:
                        logger.debug(
                            "failed to close wake-timeout response",
                            exc_info=True,
                        )
                    raise SandboxWakeTimeoutException(
                        "sandbox wake timeout: restore did not complete "
                        f"within the wake retry budget "
                        f"(attempts={wake_attempts_used}, "
                        f"waited={elapsed.total_seconds():.3f}s); the "
                        "request was never forwarded and the resume "
                        "continues server-side",
                        attempts=wake_attempts_used,
                        waited=elapsed,
                        request_id=request_id,
                        response_body=body,
                    )
                # Clamp only against budgets tighter than the wake wait
                # (general deadline / readiness deadline); the next loop
                # iteration surfaces their exhaustion.
                if policy.overall_deadline is not None:
                    remaining = policy.overall_deadline - elapsed
                    if retry_after > remaining:
                        retry_after = max(remaining, timedelta(0))
                if readiness_deadline is not None:
                    retry_after = min(
                        retry_after,
                        timedelta(
                            seconds=max(
                                0, readiness_deadline - time.monotonic()
                            )
                        ),
                    )
                self._emit_wake_retry(
                    request=request,
                    # Global attempt numbering: the upcoming attempt is
                    # one past every consumed cycle, general and wake
                    # alike, so interleaved paths never regress.
                    event=retries_used + wake_attempts_used,
                    sleep_for=retry_after,
                    request_id=response.headers.get("X-Request-ID"),
                )
                try:
                    response.close()
                except Exception:
                    logger.debug(
                        "failed to close retried response", exc_info=True
                    )
                if retry_after.total_seconds() > 0:
                    time.sleep(retry_after.total_seconds())
                continue

            if not should_retry(
                method,
                outcome,
                retries_used,
                policy,
                elapsed,
                body_replayable=body_replayable,
            ):
                if exc is not None:
                    raise exc
                assert response is not None
                return response

            last_exc = exc

            retry_after = None
            if response is not None:
                retry_after = parse_retry_after(
                    response.headers.get("Retry-After")
                )
                retry_after = apply_retry_after_cap(retry_after)

            if retry_after is not None:
                sleep_for = retry_after
            else:
                sleep_for = compute_backoff(
                    retries_used, policy, previous_sleep, self._rng
                )

            if policy.overall_deadline is not None:
                remaining = policy.overall_deadline - elapsed
                if sleep_for > remaining:
                    sleep_for = max(remaining, timedelta(0))

            if readiness_deadline is not None:
                sleep_for = min(sleep_for, timedelta(seconds=max(0, readiness_deadline - time.monotonic())))

            request_id = (
                response.headers.get("X-Request-ID") if response is not None else None
            )
            status_code = response.status_code if response is not None else None
            event = RetryEvent(
                attempt=retries_used + wake_attempts_used + 2,
                retries_used=retries_used + wake_attempts_used,
                method=method,
                url=str(request.url),
                cause=outcome.cause,
                status_code=status_code,
                backoff=sleep_for,
                request_id=request_id,
                exception=exc,
            )
            logger.warning(
                "retrying %s %s: attempt=%d cause=%s status=%s backoff=%.3fs request_id=%s",
                event.method,
                event.url,
                event.attempt,
                event.cause.value,
                event.status_code,
                event.backoff.total_seconds(),
                event.request_id,
            )
            if policy.on_retry is not None:
                try:
                    policy.on_retry(event)
                except Exception:
                    logger.exception("RetryPolicy.on_retry callback raised")

            if response is not None:
                try:
                    response.close()
                except Exception:
                    logger.debug("failed to close retried response", exc_info=True)

            retries_used += 1
            previous_sleep = sleep_for
            if sleep_for.total_seconds() > 0:
                time.sleep(sleep_for.total_seconds())

    def _emit_wake_retry(
        self,
        *,
        request: httpx.Request,
        event: int,
        sleep_for: timedelta,
        request_id: str | None,
    ) -> None:
        logger.warning(
            "retrying %s %s: attempt=%d cause=%s status=%s backoff=%.3fs request_id=%s",
            request.method,
            request.url,
            event + 1,
            RetryCause.WAKE_TIMEOUT.value,
            503,
            sleep_for.total_seconds(),
            request_id,
        )
        if self._policy.on_retry is not None:
            try:
                self._policy.on_retry(
                    RetryEvent(
                        attempt=event + 1,
                        retries_used=event,
                        method=request.method.upper(),
                        url=str(request.url),
                        cause=RetryCause.WAKE_TIMEOUT,
                        status_code=503,
                        backoff=sleep_for,
                        request_id=request_id,
                        exception=None,
                    )
                )
            except Exception:
                logger.exception("RetryPolicy.on_retry callback raised")

    def close(self) -> None:
        if self._owns_inner:
            self._inner.close()
