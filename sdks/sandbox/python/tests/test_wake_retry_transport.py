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
"""OSEP-0024 wake-retry contract tests for the retry transports."""

from __future__ import annotations

import asyncio
import random
from collections.abc import Callable
from dataclasses import replace
from datetime import timedelta
from http import HTTPStatus

import httpx
import pytest

from opensandbox.exceptions import SandboxWakeTimeoutException
from opensandbox.transport import (
    JitterMode,
    RetryAsyncTransport,
    RetryCause,
    RetryEvent,
    RetryPolicy,
    RetrySyncTransport,
)
from opensandbox.transport._classify import is_wake_timeout_response

_WAKE_BODY = (
    b'{"error": {"code": "sandbox_wake_timeout", "message": "restore still '
    b'in progress; request was never forwarded and is safe to retry", '
    b'"sandboxId": "sb_1"}}'
)


def _wake_503(
    retry_after: str | None = "1",
    body: bytes | None = _WAKE_BODY,
) -> httpx.Response:
    headers = {"OpenSandbox-Wake": "expired"}
    if retry_after is not None:
        headers["Retry-After"] = retry_after
    return httpx.Response(503, headers=headers, content=body or b"")


def _bare_503() -> httpx.Response:
    return httpx.Response(503)


def _wake_policy(
    *,
    wake_retry_enabled: bool = True,
    wake_overall_deadline: timedelta = timedelta(seconds=60),
    wake_max_attempts: int = 5,
    max_retries: int = 0,
    on_retry: Callable[[RetryEvent], None] | None = None,
) -> RetryPolicy:
    """Zero-sleep policy so tests run fast; wake knobs configurable."""
    return RetryPolicy(
        max_retries=max_retries,
        initial_backoff=timedelta(seconds=0),
        max_backoff=timedelta(seconds=0),
        jitter=JitterMode.NONE,
        wake_retry_enabled=wake_retry_enabled,
        wake_overall_deadline=wake_overall_deadline,
        wake_max_attempts=wake_max_attempts,
        on_retry=on_retry,
    )


class _ScriptAsync(httpx.AsyncBaseTransport):
    def __init__(self, script: list[httpx.Response]) -> None:
        self._script = script
        self.calls = 0
        self.methods: list[str] = []

    async def handle_async_request(
        self, request: httpx.Request
    ) -> httpx.Response:
        self.calls += 1
        self.methods.append(request.method)
        if not self._script:
            raise AssertionError("unexpected extra call")
        return self._script.pop(0)


class _ScriptSync(httpx.BaseTransport):
    def __init__(self, script: list[httpx.Response]) -> None:
        self._script = script
        self.calls = 0
        self.methods: list[str] = []

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        self.calls += 1
        self.methods.append(request.method)
        if not self._script:
            raise AssertionError("unexpected extra call")
        return self._script.pop(0)


class TestRecognition:
    def test_marker_on_503_recognized(self) -> None:
        assert is_wake_timeout_response(_wake_503())

    def test_marker_without_503_not_recognized(self) -> None:
        resp = httpx.Response(
            200, headers={"OpenSandbox-Wake": "expired"}, text="hi"
        )
        assert not is_wake_timeout_response(resp)

    def test_wrong_marker_value_not_recognized(self) -> None:
        resp = httpx.Response(503, headers={"OpenSandbox-Wake": "forged"})
        assert not is_wake_timeout_response(resp)

    def test_bare_503_not_recognized(self) -> None:
        assert not is_wake_timeout_response(_bare_503())


class TestSyncWakeRetry:
    def test_post_retried_on_marked_503_then_success(self) -> None:
        inner = _ScriptSync([_wake_503(), httpx.Response(200, text="ok")])
        rt = RetrySyncTransport(
            inner, _wake_policy(), rng=random.Random(0)
        )
        with httpx.Client(transport=rt, base_url="http://x") as client:
            resp = client.post("/", json={"cmd": "ls"})
        assert resp.status_code == 200
        assert inner.calls == 2
        assert inner.methods == ["POST", "POST"]

    def test_exhaustion_raises_typed_error(self) -> None:
        inner = _ScriptSync([_wake_503() for _ in range(5)])
        rt = RetrySyncTransport(
            inner,
            _wake_policy(wake_max_attempts=5),
            rng=random.Random(0),
        )
        with httpx.Client(transport=rt, base_url="http://x") as client:
            with pytest.raises(SandboxWakeTimeoutException) as exc_info:
                client.post("/", json={"cmd": "ls"})
        exc = exc_info.value
        assert exc.attempts == 5
        assert exc.waited >= timedelta(0)
        assert exc.status_code == 503
        assert exc.error is not None
        assert exc.error.code == "WAKE_TIMEOUT"
        assert not exc.is_retryable
        assert b"sandbox_wake_timeout" in (exc.response_body or b"")
        assert inner.calls == 5

    def test_missing_retry_after_falls_back_to_one_second(self) -> None:
        # Missing Retry-After falls back to the documented gateway value;
        # exhaustion still carries the parsed body.
        inner = _ScriptSync([_wake_503(retry_after=None)])
        rt = RetrySyncTransport(
            inner,
            _wake_policy(wake_max_attempts=1),
            rng=random.Random(0),
        )
        with httpx.Client(transport=rt, base_url="http://x") as client:
            with pytest.raises(SandboxWakeTimeoutException):
                client.post("/", json={})
        assert inner.calls == 1

    def test_disabled_wake_keeps_bare_503_semantics(self) -> None:
        inner = _ScriptSync([_wake_503()])
        rt = RetrySyncTransport(
            inner,
            _wake_policy(wake_retry_enabled=False),
            rng=random.Random(0),
        )
        with httpx.Client(transport=rt, base_url="http://x") as client:
            resp = client.post("/", json={})
        assert resp.status_code == 503
        assert inner.calls == 1

    def test_bare_503_not_retried_for_post(self) -> None:
        inner = _ScriptSync([_bare_503()])
        rt = RetrySyncTransport(
            inner, _wake_policy(), rng=random.Random(0)
        )
        with httpx.Client(transport=rt, base_url="http://x") as client:
            resp = client.post("/", json={})
        assert resp.status_code == 503
        assert inner.calls == 1

    def test_retry_after_http_date_honored(self) -> None:
        # HTTP-date in the past normalizes to zero wait; the retry still
        # happens and succeeds.
        inner = _ScriptSync(
            [
                _wake_503(retry_after="Wed, 21 Oct 2015 07:28:00 GMT"),
                httpx.Response(200),
            ]
        )
        rt = RetrySyncTransport(
            inner, _wake_policy(), rng=random.Random(0)
        )
        with httpx.Client(transport=rt, base_url="http://x") as client:
            resp = client.post("/", json={})
        assert resp.status_code == 200
        assert inner.calls == 2

    def test_on_retry_hook_receives_wake_cause(self) -> None:
        events: list[RetryEvent] = []
        inner = _ScriptSync([_wake_503(), httpx.Response(200)])
        rt = RetrySyncTransport(
            inner,
            _wake_policy(on_retry=events.append),
            rng=random.Random(0),
        )
        with httpx.Client(transport=rt, base_url="http://x") as client:
            client.post("/", json={})
        assert len(events) == 1
        assert events[0].cause == RetryCause.WAKE_TIMEOUT
        assert events[0].status_code == 503
        assert events[0].attempt == 2

    def test_wake_retry_does_not_consume_general_budget(self) -> None:
        # After a wake cycle resolves, the general idempotent retry
        # budget must still be intact for other failures.
        inner = _ScriptSync(
            [_wake_503(), _bare_503(), httpx.Response(200)]
        )
        policy = replace(
            _wake_policy(max_retries=1),
            retryable_status_codes_idempotent=frozenset(
                {HTTPStatus.SERVICE_UNAVAILABLE}
            ),
        )
        rt = RetrySyncTransport(inner, policy, rng=random.Random(0))
        with httpx.Client(transport=rt, base_url="http://x") as client:
            resp = client.get("/")
        assert resp.status_code == 200
        assert inner.calls == 3


class TestAsyncWakeRetry:
    @pytest.mark.asyncio
    async def test_cancelled_exhaustion_body_read_closes_response(self) -> None:
        started = asyncio.Event()

        class PendingBody(httpx.AsyncByteStream):
            closed = False

            async def __aiter__(self):
                started.set()
                await asyncio.Event().wait()
                yield b""

            async def aclose(self) -> None:
                self.closed = True

        stream = PendingBody()
        response = httpx.Response(
            503, headers={"OpenSandbox-Wake": "expired"}, stream=stream
        )
        inner = _ScriptAsync([response])
        transport = RetryAsyncTransport(inner, _wake_policy(wake_max_attempts=1))
        async with httpx.AsyncClient(
            transport=transport, base_url="http://x"
        ) as client:
            task = asyncio.create_task(client.post("/", json={}))
            await asyncio.wait_for(started.wait(), timeout=1)
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await task

        assert stream.closed
        assert response.is_closed
        assert inner.calls == 1

    @pytest.mark.asyncio
    async def test_post_retried_on_marked_503_then_success(self) -> None:
        inner = _ScriptAsync([_wake_503(), httpx.Response(200, text="ok")])
        rt = RetryAsyncTransport(
            inner, _wake_policy(), rng=random.Random(0)
        )
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            resp = await client.post("/", json={"cmd": "ls"})
        assert resp.status_code == 200
        assert inner.calls == 2

    @pytest.mark.asyncio
    async def test_exhaustion_raises_typed_error(self) -> None:
        inner = _ScriptAsync([_wake_503() for _ in range(3)])
        rt = RetryAsyncTransport(
            inner,
            _wake_policy(wake_max_attempts=3),
            rng=random.Random(0),
        )
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            with pytest.raises(SandboxWakeTimeoutException) as exc_info:
                await client.post("/", json={"cmd": "ls"})
        assert exc_info.value.attempts == 3
        assert inner.calls == 3

    @pytest.mark.asyncio
    async def test_disabled_wake_returns_marked_response(self) -> None:
        inner = _ScriptAsync([_wake_503()])
        rt = RetryAsyncTransport(
            inner,
            _wake_policy(wake_retry_enabled=False),
            rng=random.Random(0),
        )
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            resp = await client.post("/", json={})
        assert resp.status_code == 503
        assert resp.headers["OpenSandbox-Wake"] == "expired"
        assert inner.calls == 1


class TestBudgetBoundaries:
    def test_retry_after_exceeding_remaining_budget_stops(self) -> None:
        # The 1s Retry-After does not fit the remaining wake budget, so
        # the SDK stops after the first marked response instead of
        # overshooting the caller's wall-clock bound.
        inner = _ScriptSync([_wake_503()])
        rt = RetrySyncTransport(
            inner,
            _wake_policy(wake_overall_deadline=timedelta(microseconds=1)),
            rng=random.Random(0),
        )
        with httpx.Client(transport=rt, base_url="http://x") as client:
            with pytest.raises(SandboxWakeTimeoutException) as exc_info:
                client.post("/", json={})
        assert exc_info.value.attempts == 1
        assert inner.calls == 1

    def test_non_replayable_body_not_wake_retried(self) -> None:
        # A multipart files= body cannot be replayed; the marked 503 must
        # surface unchanged rather than resending a consumed stream.
        inner = _ScriptSync([_wake_503()])
        rt = RetrySyncTransport(
            inner, _wake_policy(), rng=random.Random(0)
        )
        with httpx.Client(transport=rt, base_url="http://x") as client:
            resp = client.post("/", files={"f": b"payload"})
        assert resp.status_code == 503
        assert inner.calls == 1

    def test_disabled_policy_does_not_wrap_transport(self) -> None:
        assert not RetryPolicy.disabled().wraps_transport()

    def test_default_policy_wraps_for_wake(self) -> None:
        assert RetryPolicy(max_retries=0).wraps_transport()

    def test_policy_validation(self) -> None:
        with pytest.raises(ValueError):
            RetryPolicy(wake_max_attempts=0)
        with pytest.raises(ValueError):
            RetryPolicy(wake_overall_deadline=timedelta(seconds=0))

    def test_disabled_disables_wake(self) -> None:
        policy = RetryPolicy.disabled()
        assert policy.wake_retry_enabled is False


class TestRetryAfterCeilingAndEventFields:
    def test_pathological_retry_after_capped_sync(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # Retry-After: 3600 must clamp to the 60s ceiling exactly like
        # the general path; observe via on_retry instead of sleeping.
        import opensandbox.transport._sync_retry as sync_retry_mod

        events: list[RetryEvent] = []
        sleeps: list[float] = []
        monkeypatch.setattr(sync_retry_mod.time, "sleep", sleeps.append)
        inner = _ScriptSync(
            [_wake_503(retry_after="3600"), httpx.Response(200)]
        )
        rt = RetrySyncTransport(
            inner,
            _wake_policy(
                # Budget wide enough that the capped 60s wait fits, so
                # the capped value is observable on the retry event.
                wake_overall_deadline=timedelta(seconds=120),
                wake_max_attempts=2,
                on_retry=events.append,
            ),
            rng=random.Random(0),
        )
        with httpx.Client(transport=rt, base_url="http://x") as client:
            resp = client.post("/", json={})
        assert resp.status_code == 200
        assert inner.calls == 2
        assert events[0].backoff == timedelta(seconds=60)
        assert sleeps == [60.0]

    @pytest.mark.asyncio
    async def test_pathological_retry_after_capped_async(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        import opensandbox.transport._async_retry as async_retry_mod

        events: list[RetryEvent] = []
        sleeps: list[float] = []

        async def _no_sleep(seconds: float) -> None:
            sleeps.append(seconds)

        monkeypatch.setattr(async_retry_mod.asyncio, "sleep", _no_sleep)
        inner = _ScriptAsync(
            [_wake_503(retry_after="3600"), httpx.Response(200)]
        )
        rt = RetryAsyncTransport(
            inner,
            _wake_policy(
                # Budget wide enough that the capped 60s wait fits, so
                # the capped value is observable on the retry event.
                wake_overall_deadline=timedelta(seconds=120),
                wake_max_attempts=2,
                on_retry=events.append,
            ),
            rng=random.Random(0),
        )
        async with httpx.AsyncClient(
            transport=rt, base_url="http://x"
        ) as client:
            resp = await client.post("/", json={})
        assert resp.status_code == 200
        assert inner.calls == 2
        assert events[0].backoff == timedelta(seconds=60)
        assert sleeps == [60.0]

    def test_no_deadline_relies_on_cap_and_attempt_count(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # wake_overall_deadline=None is valid: the per-sleep 60s cap
        # plus wake_max_attempts keep every wait bounded.
        import opensandbox.transport._sync_retry as sync_retry_mod

        monkeypatch.setattr(sync_retry_mod.time, "sleep", lambda s: None)
        inner = _ScriptSync(
            [_wake_503(), _wake_503(), httpx.Response(200)]
        )
        rt = RetrySyncTransport(
            inner,
            _wake_policy(wake_overall_deadline=None, wake_max_attempts=3),
            rng=random.Random(0),
        )
        with httpx.Client(transport=rt, base_url="http://x") as client:
            resp = client.post("/", json={})
        assert resp.status_code == 200
        assert inner.calls == 3

    def test_wake_event_carries_request_id(self) -> None:
        events: list[RetryEvent] = []
        marked = httpx.Response(
            503,
            headers={
                "OpenSandbox-Wake": "expired",
                "Retry-After": "1",
                "X-Request-ID": "req-42",
            },
            content=_WAKE_BODY,
        )
        inner = _ScriptSync([marked, httpx.Response(200)])
        rt = RetrySyncTransport(
            inner,
            _wake_policy(wake_max_attempts=2, on_retry=events.append),
            rng=random.Random(0),
        )
        with httpx.Client(transport=rt, base_url="http://x") as client:
            client.post("/", json={})
        assert events[0].request_id == "req-42"

    def test_attempt_numbering_monotonic_across_interleaved_retries(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # wake 503 -> bare 503 (general retry) -> wake 503 -> 200:
        # attempt numbers must never regress and retries_used must
        # count every consumed cycle.
        from dataclasses import replace as _replace
        from http import HTTPStatus as _HS

        import opensandbox.transport._sync_retry as sync_retry_mod

        monkeypatch.setattr(sync_retry_mod.time, "sleep", lambda s: None)
        events: list[RetryEvent] = []
        inner = _ScriptSync(
            [
                _wake_503(),
                _bare_503(),
                _wake_503(),
                httpx.Response(200),
            ]
        )
        policy = _replace(
            _wake_policy(
                max_retries=1,
                wake_max_attempts=3,
                on_retry=events.append,
            ),
            retryable_status_codes_idempotent=frozenset(
                {_HS.SERVICE_UNAVAILABLE}
            ),
        )
        rt = RetrySyncTransport(inner, policy, rng=random.Random(0))
        with httpx.Client(transport=rt, base_url="http://x") as client:
            resp = client.get("/")
        assert resp.status_code == 200
        assert inner.calls == 4
        assert [e.attempt for e in events] == [2, 3, 4]
        assert [e.retries_used for e in events] == [1, 1, 3]
        assert [e.cause for e in events] == [
            RetryCause.WAKE_TIMEOUT,
            RetryCause.STATUS_503,
            RetryCause.WAKE_TIMEOUT,
        ]
