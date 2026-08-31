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
"""Sync httpx transport wrapper enforcing :class:`RetryPolicy`."""

from __future__ import annotations

import logging
import random
import time
from datetime import timedelta

import httpx

from opensandbox.transport._async_retry import (
    _capture_baseline_timeout,
    _clamp_attempt_timeout,
)
from opensandbox.transport._classify import (
    classify_transport_exception,
    is_body_replayable,
    outcome_for_response,
)
from opensandbox.transport._decision import (
    Outcome,
    apply_retry_after_cap,
    compute_backoff,
    parse_retry_after,
    should_retry,
)
from opensandbox.transport.retry import RetryEvent, RetryPolicy

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
        """The wrapped raw transport, safe to hand to SSE clients."""
        return self._inner

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        policy = self._policy
        method = request.method.upper()
        start = time.monotonic()
        previous_sleep = timedelta(0)
        retries_used = 0
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
            # The terminating condition is the deadline, so always raise
            # a timeout (mapped to SandboxTimeoutException by the
            # converter) even when the last attempt failed for another
            # reason; that failure is preserved as the cause for context.
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
            # The sync path cannot wrap the transport call in a
            # wall-clock deadline (no equivalent of asyncio.wait_for
            # without extra threads), so per-phase clamps written by
            # _clamp_attempt_timeout are the enforcement mechanism. A
            # pathological server that keeps sending body chunks under
            # the read timeout can still exceed overall_deadline; the
            # loop re-checks and bails out on the next iteration.
            try:
                response = self._inner.handle_request(request)
                outcome = outcome_for_response(response)
            except httpx.TransportError as e:
                exc = e
                outcome = classify_transport_exception(e)

            elapsed = timedelta(seconds=time.monotonic() - start)
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

            # Clamp the sleep to the remaining overall deadline; the
            # next loop iteration will exit via should_retry().
            if policy.overall_deadline is not None:
                remaining = policy.overall_deadline - elapsed
                if sleep_for > remaining:
                    sleep_for = max(remaining, timedelta(0))

            request_id = (
                response.headers.get("X-Request-ID") if response is not None else None
            )
            status_code = response.status_code if response is not None else None
            event = RetryEvent(
                attempt=retries_used + 2,
                retries_used=retries_used,
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

    def close(self) -> None:
        if self._owns_inner:
            self._inner.close()
