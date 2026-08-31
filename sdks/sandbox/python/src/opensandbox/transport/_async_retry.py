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
"""Async httpx transport wrapper enforcing :class:`RetryPolicy`."""

from __future__ import annotations

import asyncio
import logging
import random
import time
from datetime import timedelta

import httpx

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


def _clamp_attempt_timeout(
    request: httpx.Request,
    policy: RetryPolicy,
    baseline_timeout: dict[str, float | None] | None,
    remaining_deadline: timedelta | None,
) -> None:
    """
    Bound the current attempt by ``per_attempt_timeout`` and the remaining
    ``overall_deadline``, whichever is tighter.

    httpx propagates ``request.extensions["timeout"]`` down to httpcore
    for connect/read/write/pool. The request object is reused across
    retries, so we rebuild the timeout dict from the enclosing client's
    baseline (captured once before the loop) and clamp each phase down
    to ``min(per_attempt_timeout, remaining_deadline)`` when either is
    set.
    """
    limit: float | None = None
    if policy.per_attempt_timeout is not None:
        limit = policy.per_attempt_timeout.total_seconds()
    if remaining_deadline is not None:
        remaining_s = max(remaining_deadline.total_seconds(), 0.0)
        limit = remaining_s if limit is None else min(limit, remaining_s)

    if limit is None:
        # Nothing to do: neither knob is set. Preserve whatever the
        # enclosing client stamped on the request.
        return

    timeout: dict[str, float | None] = dict(baseline_timeout or {})
    for phase in ("connect", "read", "write", "pool"):
        existing = timeout.get(phase)
        if existing is None:
            timeout[phase] = limit
        else:
            timeout[phase] = min(existing, limit)
    request.extensions["timeout"] = timeout


def _capture_baseline_timeout(
    request: httpx.Request,
) -> dict[str, float | None] | None:
    """Snapshot the client-provided timeout once before the retry loop."""
    baseline = request.extensions.get("timeout")
    if isinstance(baseline, dict):
        return dict(baseline)
    return None


class RetryAsyncTransport(httpx.AsyncBaseTransport):
    """
    Wraps an inner async transport with retry semantics.

    Does not own the inner transport by default; :meth:`aclose` only
    delegates when ``owns_inner=True``.
    """

    def __init__(
        self,
        inner: httpx.AsyncBaseTransport,
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
    def inner(self) -> httpx.AsyncBaseTransport:
        """The wrapped raw transport, safe to hand to SSE clients."""
        return self._inner

    async def handle_async_request(
        self, request: httpx.Request
    ) -> httpx.Response:
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
            # Bail out before dispatching if the wall-clock budget is
            # already gone: otherwise we would send an attempt with a
            # zero per-phase timeout and possibly trigger a spurious
            # retry. Any previously retried response has already been
            # aclose()d below, so we can only surface an exception here.
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
            # Wrap the transport call in a wall-clock deadline whenever
            # the caller set per_attempt_timeout or overall_deadline.
            # httpx's connect/read/write/pool timeouts are per-phase, so
            # this bounds the header phase. Body reads happen in
            # AsyncClient.send after the transport returns and remain
            # subject to the per-phase read timeout only; that is the
            # deliberate trade-off for keeping SSE clients on the same
            # transport (draining the body here would kill the stream).
            attempt_deadline: float | None = None
            if remaining is not None:
                attempt_deadline = max(remaining.total_seconds(), 0.0)
            if policy.per_attempt_timeout is not None:
                pat_s = policy.per_attempt_timeout.total_seconds()
                attempt_deadline = (
                    pat_s if attempt_deadline is None else min(attempt_deadline, pat_s)
                )
            try:
                if attempt_deadline is None:
                    response = await self._inner.handle_async_request(request)
                else:
                    response = await asyncio.wait_for(
                        self._inner.handle_async_request(request),
                        timeout=attempt_deadline,
                    )
                outcome = outcome_for_response(response)
            except asyncio.CancelledError:
                # Terminal; never swallow.
                raise
            except asyncio.TimeoutError as e:
                # wait_for hit its deadline before the transport call
                # (or the wrapper-driven body read) finished. Surface as
                # a post-send read timeout so the retry decision matches
                # httpx.ReadTimeout semantics.
                exc = httpx.ReadTimeout(
                    "per-attempt deadline exceeded"
                )
                exc.__cause__ = e
                outcome = classify_transport_exception(exc)
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

            # Compute backoff: honor Retry-After when present.
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

            # Clamp the sleep to the remaining overall deadline so a long
            # Retry-After or a long computed backoff cannot push us past
            # the caller's wall-clock budget. The next loop iteration
            # will exit via should_retry() when the deadline is reached.
            if policy.overall_deadline is not None:
                remaining = policy.overall_deadline - elapsed
                if sleep_for > remaining:
                    sleep_for = max(remaining, timedelta(0))

            # Emit event before sleeping. The retried response body is
            # released below so the connection returns to the pool.
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
                    await response.aclose()
                except Exception:
                    logger.debug("failed to close retried response", exc_info=True)

            retries_used += 1
            previous_sleep = sleep_for
            if sleep_for.total_seconds() > 0:
                await asyncio.sleep(sleep_for.total_seconds())

    async def aclose(self) -> None:
        if self._owns_inner:
            await self._inner.aclose()
