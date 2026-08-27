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
"""Integration tests for RetryAsyncTransport / RetrySyncTransport."""

from __future__ import annotations

import random
from collections.abc import Callable
from datetime import timedelta
from http import HTTPStatus

import httpx
import pytest

from opensandbox.transport import (
    JitterMode,
    RetryAsyncTransport,
    RetryEvent,
    RetryPolicy,
    RetrySyncTransport,
)


class _CountingAsyncTransport(httpx.AsyncBaseTransport):
    """Async httpx transport that plays a scripted response sequence."""

    def __init__(
        self, scripted: list[httpx.Response | Callable[[], httpx.Response]]
    ) -> None:
        self._script = list(scripted)
        self.calls = 0

    async def handle_async_request(
        self, request: httpx.Request
    ) -> httpx.Response:
        self.calls += 1
        if not self._script:
            raise AssertionError(
                f"unexpected extra call #{self.calls} to transport"
            )
        item = self._script.pop(0)
        if callable(item):
            item = item()
        if isinstance(item, BaseException):  # pragma: no cover - defensive
            raise item
        return item


class _CountingSyncTransport(httpx.BaseTransport):
    def __init__(
        self, scripted: list[httpx.Response | Callable[[], httpx.Response]]
    ) -> None:
        self._script = list(scripted)
        self.calls = 0

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        self.calls += 1
        if not self._script:
            raise AssertionError(
                f"unexpected extra call #{self.calls} to transport"
            )
        item = self._script.pop(0)
        if callable(item):
            item = item()
        if isinstance(item, BaseException):  # pragma: no cover - defensive
            raise item
        return item


def _raise(exc: BaseException) -> Callable[[], httpx.Response]:
    def _factory() -> httpx.Response:
        raise exc

    return _factory


def _fast_policy(
    *,
    max_retries: int = 3,
    retryable_status_codes_non_idempotent: frozenset[HTTPStatus] = frozenset(),
    on_retry: Callable[[RetryEvent], None] | None = None,
    overall_deadline: timedelta | None = None,
) -> RetryPolicy:
    """Tests need near-zero sleeps; use a tiny backoff to keep runs fast."""
    return RetryPolicy(
        max_retries=max_retries,
        initial_backoff=timedelta(seconds=0),
        max_backoff=timedelta(seconds=0),
        backoff_multiplier=2.0,
        retryable_status_codes_non_idempotent=retryable_status_codes_non_idempotent,
        on_retry=on_retry,
        overall_deadline=overall_deadline,
    )


class TestAsyncTransport:
    @pytest.mark.asyncio
    async def test_retry_get_on_503_then_success(self) -> None:
        inner = _CountingAsyncTransport(
            [
                httpx.Response(503),
                httpx.Response(503),
                httpx.Response(200, text="ok"),
            ]
        )
        rt = RetryAsyncTransport(inner, _fast_policy(), rng=random.Random(0))
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            resp = await client.get("/")
        assert resp.status_code == 200
        assert inner.calls == 3

    @pytest.mark.asyncio
    async def test_do_not_retry_post_on_503(self) -> None:
        inner = _CountingAsyncTransport([httpx.Response(503)])
        rt = RetryAsyncTransport(inner, _fast_policy())
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            resp = await client.post("/", json={})
        assert resp.status_code == 503
        assert inner.calls == 1

    @pytest.mark.asyncio
    async def test_non_replayable_body_not_retried_on_status(self) -> None:
        # POST opted into 503 retry, but a multipart (files=) body is
        # not replayable: the wrapper must not attempt a resend, which
        # would raise httpx.StreamConsumed / send a truncated body.
        inner = _CountingAsyncTransport(
            [httpx.Response(503), httpx.Response(200)]
        )
        policy = _fast_policy(
            retryable_status_codes_non_idempotent=frozenset(
                {HTTPStatus.SERVICE_UNAVAILABLE}
            )
        )
        rt = RetryAsyncTransport(inner, policy)
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            resp = await client.post("/", files={"f": ("n.txt", b"data")})
        # Single attempt, no StreamConsumed.
        assert resp.status_code == 503
        assert inner.calls == 1

    @pytest.mark.asyncio
    async def test_replayable_bytes_body_still_retried_on_status(self) -> None:
        # A plain bytes body is replayable, so the opt-in status retry
        # proceeds normally.
        inner = _CountingAsyncTransport(
            [httpx.Response(503), httpx.Response(200)]
        )
        policy = _fast_policy(
            retryable_status_codes_non_idempotent=frozenset(
                {HTTPStatus.SERVICE_UNAVAILABLE}
            )
        )
        rt = RetryAsyncTransport(inner, policy)
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            resp = await client.post("/", content=b"payload")
        assert resp.status_code == 200
        assert inner.calls == 2

    @pytest.mark.asyncio
    async def test_do_not_retry_get_on_500_504_and_4xx(self) -> None:
        # 500 and 504 are outside the default retry set; 4xx are not
        # retryable by design.
        for code in (400, 401, 403, 404, 409, 500, 501, 504):
            inner = _CountingAsyncTransport([httpx.Response(code)])
            rt = RetryAsyncTransport(inner, _fast_policy())
            async with httpx.AsyncClient(
                transport=rt, base_url="http://x"
            ) as client:
                resp = await client.get("/")
            assert resp.status_code == code
            assert inner.calls == 1, f"unexpected retry on {code}"

    @pytest.mark.asyncio
    async def test_retry_after_clamped_to_60_seconds(self) -> None:
        # A pathological Retry-After: 3600 must clamp to the built-in
        # 60-second ceiling. Observe via on_retry instead of sleeping.
        events: list[RetryEvent] = []
        inner = _CountingAsyncTransport(
            [
                httpx.Response(429, headers={"Retry-After": "3600"}),
                httpx.Response(200),
            ]
        )
        policy = _fast_policy(on_retry=lambda e: events.append(e))
        # Intercept sleep so the test does not actually wait 60s.
        import opensandbox.transport._async_retry as async_retry_mod

        original_sleep = async_retry_mod.asyncio.sleep

        async def _no_sleep(_: float) -> None:
            return None

        async_retry_mod.asyncio.sleep = _no_sleep  # type: ignore[assignment]
        try:
            rt = RetryAsyncTransport(inner, policy)
            async with httpx.AsyncClient(
                transport=rt, base_url="http://x"
            ) as client:
                resp = await client.get("/")
        finally:
            async_retry_mod.asyncio.sleep = original_sleep  # type: ignore[assignment]
        assert resp.status_code == 200
        assert inner.calls == 2
        assert len(events) == 1
        assert events[0].backoff == timedelta(seconds=60)

    @pytest.mark.asyncio
    async def test_budget_exhaustion_returns_last_response(self) -> None:
        inner = _CountingAsyncTransport(
            [httpx.Response(503) for _ in range(4)]
        )
        rt = RetryAsyncTransport(inner, _fast_policy())
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            resp = await client.get("/")
        assert resp.status_code == 503
        # Total attempts = 1 initial + 3 retries.
        assert inner.calls == 4

    @pytest.mark.asyncio
    async def test_retry_get_on_connect_error(self) -> None:
        inner = _CountingAsyncTransport(
            [
                _raise(httpx.ConnectError("dns fail")),
                httpx.Response(200, text="ok"),
            ]
        )
        rt = RetryAsyncTransport(inner, _fast_policy())
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            resp = await client.get("/")
        assert resp.status_code == 200
        assert inner.calls == 2

    @pytest.mark.asyncio
    async def test_retry_post_on_pre_send_failure(self) -> None:
        """Pre-send failures are safe on any method."""
        inner = _CountingAsyncTransport(
            [
                _raise(httpx.ConnectError("connection refused")),
                httpx.Response(200, text="ok"),
            ]
        )
        rt = RetryAsyncTransport(inner, _fast_policy())
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            resp = await client.post("/", json={})
        assert resp.status_code == 200
        assert inner.calls == 2

    @pytest.mark.asyncio
    async def test_do_not_retry_post_on_read_timeout(self) -> None:
        inner = _CountingAsyncTransport(
            [_raise(httpx.ReadTimeout("read timeout"))]
        )
        rt = RetryAsyncTransport(inner, _fast_policy())
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            with pytest.raises(httpx.ReadTimeout):
                await client.post("/", json={})
        assert inner.calls == 1

    @pytest.mark.asyncio
    async def test_opt_in_lifts_post_on_selected_status(self) -> None:
        inner = _CountingAsyncTransport(
            [httpx.Response(502), httpx.Response(200)]
        )
        policy = _fast_policy(
            retryable_status_codes_non_idempotent=frozenset(
                {HTTPStatus.BAD_GATEWAY}
            )
        )
        rt = RetryAsyncTransport(inner, policy)
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            resp = await client.post("/", json={})
        assert resp.status_code == 200
        assert inner.calls == 2

    @pytest.mark.asyncio
    async def test_on_retry_callback_fires_per_retry(self) -> None:
        events: list[RetryEvent] = []
        inner = _CountingAsyncTransport(
            [httpx.Response(503), httpx.Response(503), httpx.Response(200)]
        )
        rt = RetryAsyncTransport(
            inner, _fast_policy(on_retry=lambda e: events.append(e))
        )
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            await client.get("/")
        assert len(events) == 2
        assert [e.attempt for e in events] == [2, 3]
        assert [e.retries_used for e in events] == [0, 1]
        assert events[0].status_code == 503

    @pytest.mark.asyncio
    async def test_on_retry_callback_exception_swallowed(self) -> None:
        def _boom(_: RetryEvent) -> None:
            raise RuntimeError("bad callback")

        inner = _CountingAsyncTransport(
            [httpx.Response(503), httpx.Response(200)]
        )
        rt = RetryAsyncTransport(inner, _fast_policy(on_retry=_boom))
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            resp = await client.get("/")
        assert resp.status_code == 200

    @pytest.mark.asyncio
    async def test_per_attempt_timeout_clamps_each_attempt(self) -> None:
        # With a large enclosing client timeout, per_attempt_timeout must
        # tighten each attempt's connect/read/write/pool timeout.
        seen_timeouts: list[dict[str, float]] = []

        class _Capture(httpx.AsyncBaseTransport):
            async def handle_async_request(
                self, request: httpx.Request
            ) -> httpx.Response:
                seen_timeouts.append(dict(request.extensions.get("timeout") or {}))
                return httpx.Response(503) if len(seen_timeouts) < 2 else httpx.Response(200)

        inner = _Capture()
        policy = RetryPolicy(
            max_retries=3,
            initial_backoff=timedelta(0),
            max_backoff=timedelta(0),
            per_attempt_timeout=timedelta(seconds=7),
        )
        rt = RetryAsyncTransport(inner, policy)
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x", timeout=30.0
        ) as client:
            resp = await client.get("/")
        assert resp.status_code == 200
        assert len(seen_timeouts) == 2
        for t in seen_timeouts:
            assert t["connect"] == 7.0
            assert t["read"] == 7.0
            assert t["write"] == 7.0
            assert t["pool"] == 7.0

    @pytest.mark.asyncio
    async def test_overall_deadline_exhausted_short_circuits_next_attempt(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # After a retryable failure + a Retry-After sleep that consumes
        # the deadline, the wrapper must NOT dispatch another attempt
        # with a zero per-phase timeout. Simulate a monotonic clock
        # jump so the deadline check bites without an actual sleep.
        inner = _CountingAsyncTransport(
            [
                httpx.Response(429, headers={"Retry-After": "60"}),
                # A second attempt would burn extra time; assert we
                # never reach it.
                httpx.Response(200),
            ]
        )
        policy = RetryPolicy(
            max_retries=3,
            initial_backoff=timedelta(0),
            max_backoff=timedelta(0),
            overall_deadline=timedelta(seconds=1),
        )

        import opensandbox.transport._async_retry as async_retry_mod

        real_monotonic = async_retry_mod.time.monotonic
        base = real_monotonic()
        clock = {"advance": 0.0}

        def _fake_monotonic() -> float:
            return base + clock["advance"]

        async def _sleep_that_advances(seconds: float) -> None:
            # Move the fake clock forward by the requested sleep so the
            # next loop iteration sees a real elapsed delta.
            clock["advance"] += seconds

        monkeypatch.setattr(async_retry_mod.time, "monotonic", _fake_monotonic)
        monkeypatch.setattr(async_retry_mod.asyncio, "sleep", _sleep_that_advances)

        rt = RetryAsyncTransport(inner, policy)
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x", timeout=30.0
        ) as client:
            with pytest.raises(httpx.ReadTimeout):
                await client.get("/")
        assert inner.calls == 1

    @pytest.mark.asyncio
    async def test_overall_deadline_after_connect_error_raises_timeout(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # A pre-send ConnectError is retryable, but if the deadline is
        # consumed during backoff the operation terminated *because of
        # the deadline*: surface a ReadTimeout (-> SandboxTimeoutException)
        # rather than re-raising the ConnectError, preserving the
        # ConnectError as the cause.
        connect_error = httpx.ConnectError("dns down")
        inner = _CountingAsyncTransport([_raise(connect_error)])
        policy = RetryPolicy(
            max_retries=3,
            initial_backoff=timedelta(seconds=5),
            max_backoff=timedelta(seconds=5),
            jitter=JitterMode.NONE,
            overall_deadline=timedelta(seconds=1),
        )

        import opensandbox.transport._async_retry as async_retry_mod

        real_monotonic = async_retry_mod.time.monotonic
        base = real_monotonic()
        clock = {"advance": 0.0}

        def _fake_monotonic() -> float:
            return base + clock["advance"]

        async def _sleep_that_advances(seconds: float) -> None:
            clock["advance"] += seconds

        monkeypatch.setattr(async_retry_mod.time, "monotonic", _fake_monotonic)
        monkeypatch.setattr(
            async_retry_mod.asyncio, "sleep", _sleep_that_advances
        )

        rt = RetryAsyncTransport(inner, policy)
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x", timeout=30.0
        ) as client:
            with pytest.raises(httpx.ReadTimeout) as excinfo:
                await client.get("/")
        assert excinfo.value.__cause__ is connect_error
        assert inner.calls == 1

    @pytest.mark.asyncio
    async def test_per_attempt_timeout_wraps_hung_attempt_in_wait_for(self) -> None:
        # A misbehaving inner transport that hangs forever must be
        # aborted by the wrapper's wall-clock per-attempt deadline
        # instead of stalling on httpx's per-phase timeouts.
        import asyncio

        class _Hanging(httpx.AsyncBaseTransport):
            def __init__(self) -> None:
                self.calls = 0

            async def handle_async_request(
                self, request: httpx.Request
            ) -> httpx.Response:
                self.calls += 1
                await asyncio.sleep(60)  # would blow the test's timeout
                raise AssertionError("unreachable")

        inner = _Hanging()
        policy = RetryPolicy(
            max_retries=0,
            per_attempt_timeout=timedelta(milliseconds=50),
        )
        rt = RetryAsyncTransport(inner, policy)
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x", timeout=30.0
        ) as client:
            with pytest.raises(httpx.ReadTimeout):
                await client.get("/")
        assert inner.calls == 1

    @pytest.mark.asyncio
    async def test_overall_deadline_clamps_first_attempt_timeout(self) -> None:
        # A short overall_deadline must bound the *first* attempt too,
        # not just wait for it to finish and check after the fact.
        seen_timeouts: list[dict[str, float]] = []

        class _Capture(httpx.AsyncBaseTransport):
            async def handle_async_request(
                self, request: httpx.Request
            ) -> httpx.Response:
                seen_timeouts.append(dict(request.extensions.get("timeout") or {}))
                return httpx.Response(200)

        inner = _Capture()
        policy = RetryPolicy(
            max_retries=3,
            initial_backoff=timedelta(0),
            max_backoff=timedelta(0),
            overall_deadline=timedelta(seconds=2),
        )
        rt = RetryAsyncTransport(inner, policy)
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x", timeout=30.0
        ) as client:
            resp = await client.get("/")
        assert resp.status_code == 200
        assert len(seen_timeouts) == 1
        t = seen_timeouts[0]
        # The first attempt's per-phase timeout must be clamped by the
        # remaining deadline (~2s), not the enclosing client's 30s.
        assert t["connect"] <= 2.0
        assert t["read"] <= 2.0
        assert t["write"] <= 2.0
        assert t["pool"] <= 2.0

    @pytest.mark.asyncio
    async def test_overall_deadline_clamps_retry_after_sleep(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # Retry-After of 3600 must be clamped by the remaining deadline
        # instead of stalling for the full 60-second ceiling.
        events: list[RetryEvent] = []
        inner = _CountingAsyncTransport(
            [
                httpx.Response(429, headers={"Retry-After": "3600"}),
                httpx.Response(200),
            ]
        )
        policy = RetryPolicy(
            max_retries=3,
            initial_backoff=timedelta(0),
            max_backoff=timedelta(0),
            overall_deadline=timedelta(seconds=2),
            on_retry=lambda e: events.append(e),
        )
        # Skip the real sleep so the test does not stall for 2 s.
        import opensandbox.transport._async_retry as async_retry_mod

        async def _no_sleep(_: float) -> None:
            return None

        monkeypatch.setattr(async_retry_mod.asyncio, "sleep", _no_sleep)

        rt = RetryAsyncTransport(inner, policy)
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            resp = await client.get("/")
        assert resp.status_code == 200
        assert len(events) == 1
        assert events[0].backoff <= timedelta(seconds=2)


class TestSyncTransport:
    def test_retry_get_on_503_then_success(self) -> None:
        inner = _CountingSyncTransport(
            [httpx.Response(503), httpx.Response(200)]
        )
        rt = RetrySyncTransport(inner, _fast_policy())
        with httpx.Client(transport=rt, base_url="http://x") as client:
            resp = client.get("/")
        assert resp.status_code == 200
        assert inner.calls == 2

    def test_do_not_retry_post_on_503(self) -> None:
        inner = _CountingSyncTransport([httpx.Response(503)])
        rt = RetrySyncTransport(inner, _fast_policy())
        with httpx.Client(transport=rt, base_url="http://x") as client:
            resp = client.post("/", json={})
        assert resp.status_code == 503
        assert inner.calls == 1

    def test_retry_post_on_connect_error(self) -> None:
        inner = _CountingSyncTransport(
            [_raise(httpx.ConnectError("fail")), httpx.Response(200)]
        )
        rt = RetrySyncTransport(inner, _fast_policy())
        with httpx.Client(transport=rt, base_url="http://x") as client:
            resp = client.post("/", json={})
        assert resp.status_code == 200
        assert inner.calls == 2

    def test_overall_deadline_after_connect_error_raises_timeout(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # Sync counterpart: deadline consumed during backoff after a
        # retryable ConnectError must surface a ReadTimeout with the
        # ConnectError preserved as the cause.
        connect_error = httpx.ConnectError("dns down")
        inner = _CountingSyncTransport([_raise(connect_error)])
        policy = RetryPolicy(
            max_retries=3,
            initial_backoff=timedelta(seconds=5),
            max_backoff=timedelta(seconds=5),
            jitter=JitterMode.NONE,
            overall_deadline=timedelta(seconds=1),
        )

        import opensandbox.transport._sync_retry as sync_retry_mod

        real_monotonic = sync_retry_mod.time.monotonic
        base = real_monotonic()
        clock = {"advance": 0.0}

        def _fake_monotonic() -> float:
            return base + clock["advance"]

        def _sleep_that_advances(seconds: float) -> None:
            clock["advance"] += seconds

        monkeypatch.setattr(sync_retry_mod.time, "monotonic", _fake_monotonic)
        monkeypatch.setattr(sync_retry_mod.time, "sleep", _sleep_that_advances)

        rt = RetrySyncTransport(inner, policy)
        with httpx.Client(transport=rt, base_url="http://x", timeout=30.0) as client:
            with pytest.raises(httpx.ReadTimeout) as excinfo:
                client.get("/")
        assert excinfo.value.__cause__ is connect_error
        assert inner.calls == 1


class TestUnwrapRetryTransport:
    def test_unwraps_async_retry_transport(self) -> None:
        from opensandbox.transport import unwrap_retry_transport

        inner = httpx.AsyncHTTPTransport()
        wrapper = RetryAsyncTransport(inner, RetryPolicy(), owns_inner=True)
        assert unwrap_retry_transport(wrapper) is inner

    def test_unwraps_sync_retry_transport(self) -> None:
        from opensandbox.transport import unwrap_retry_transport

        inner = httpx.HTTPTransport()
        wrapper = RetrySyncTransport(inner, RetryPolicy(), owns_inner=True)
        assert unwrap_retry_transport(wrapper) is inner

    def test_passes_through_raw_transport(self) -> None:
        from opensandbox.transport import unwrap_retry_transport

        raw = httpx.AsyncHTTPTransport()
        assert unwrap_retry_transport(raw) is raw

    def test_passes_through_user_mock(self) -> None:
        # Mock transports used in unit tests (no `inner` attribute) must
        # round-trip unchanged so SSE mock-injection keeps working.
        from opensandbox.transport import unwrap_retry_transport

        class _Mock(httpx.AsyncBaseTransport):
            async def handle_async_request(
                self, request: httpx.Request
            ) -> httpx.Response:
                return httpx.Response(200)

        m = _Mock()
        assert unwrap_retry_transport(m) is m

    def test_passes_through_none(self) -> None:
        from opensandbox.transport import unwrap_retry_transport

        assert unwrap_retry_transport(None) is None
