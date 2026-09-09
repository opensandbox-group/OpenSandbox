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
import asyncio
import time
from collections.abc import Awaitable, Callable
from contextvars import ContextVar
from datetime import timedelta
from typing import Any, TypeVar

import httpx

from opensandbox.exceptions import SandboxApiException, SandboxReadyTimeoutException
from opensandbox.transport._deadline_sync import DEADLINE_EXTENSION

T = TypeVar("T")
_cancelled_tasks: set[asyncio.Future[Any]] = set()


def _observe_cancelled_task(task: asyncio.Future[Any]) -> None:
    _cancelled_tasks.discard(task)
    if not task.cancelled():
        task.exception()


_sync_budget: ContextVar["ReadinessBudget | None"] = ContextVar(
    "readiness_budget", default=None
)


def constrain_readiness_request(request: httpx.Request) -> None:
    budget = _sync_budget.get()
    if budget is not None:
        remaining = budget.remaining()
        request.extensions[DEADLINE_EXTENSION] = budget.deadline
        request.extensions["timeout"] = {
            key: min(value, remaining) if value is not None else remaining
            for key, value in request.extensions.get("timeout", {}).items()
        }


class ReadinessBudget:
    def __init__(self, timeout: timedelta, interval: timedelta) -> None:
        self.timeout = timeout
        self.context: str | None = None
        self.attempts = 0
        self.deadline = time.monotonic() + timeout.total_seconds()
        self.interval = interval.total_seconds()
        self.last_error: Exception | None = None

    def expired(self) -> SandboxReadyTimeoutException:
        detail = (
            "Health check has not succeeded"
            if self.context is not None
            else "Endpoint has not been resolved"
        )
        if self.last_error is not None:
            detail = f"Last error: {self.last_error}"
        message = (
            f"Sandbox health check timed out after {self.timeout.total_seconds()}s "
            f"({self.attempts} attempts). {detail}. {self.context}."
            if self.context is not None
            else f"Sandbox readiness timed out. {detail}"
        )
        return SandboxReadyTimeoutException(message, cause=self.last_error)

    def remaining(self) -> float:
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise self.expired()
        return remaining

    @staticmethod
    def retryable(error: Exception) -> bool:
        return (
            isinstance(error, SandboxApiException)
            and error.status_code == 404
            and error.error.code == "KUBERNETES::POD_IP_NOT_AVAILABLE"
        )

    async def run(self, action: Callable[[], Awaitable[T]]) -> T:
        remaining = self.remaining()
        task = asyncio.ensure_future(action())
        try:
            done, _ = await asyncio.wait({task}, timeout=remaining)
            if not done:
                raise self.expired()
            result = task.result()
            self.remaining()
            return result
        finally:
            if not task.done():
                task.cancel()
            _cancelled_tasks.add(task)
            task.add_done_callback(_observe_cancelled_task)

    async def endpoint(self, action: Callable[[], Awaitable[T]]) -> T:
        while True:
            try:
                return await self.run(action)
            except Exception as error:
                if not self.retryable(error):
                    raise
                self.last_error = error
            await asyncio.sleep(min(self.interval, self.remaining()))

    async def health(self, action: Callable[[], Awaitable[bool]], context: str) -> None:
        self.context = context
        self.last_error = None
        while True:
            try:
                self.attempts += 1
                if await self.run(action):
                    return
                self.last_error = None
            except Exception as error:
                self.remaining()
                self.last_error = error
            await asyncio.sleep(min(self.interval, self.remaining()))

    def run_sync(self, action: Callable[[], T]) -> T:
        self.remaining()
        token = _sync_budget.set(self)
        try:
            result = action()
        except Exception as error:
            if self.last_error is None:
                self.last_error = error
            self.remaining()
            raise
        finally:
            _sync_budget.reset(token)
        self.remaining()
        return result

    def endpoint_sync(self, action: Callable[[], T]) -> T:
        while True:
            try:
                return self.run_sync(action)
            except Exception as error:
                if not self.retryable(error):
                    raise
                self.last_error = error
            time.sleep(min(self.interval, self.remaining()))

    def health_sync(self, action: Callable[[], bool], context: str) -> None:
        self.context = context
        self.last_error = None
        while True:
            try:
                self.attempts += 1
                if self.run_sync(action):
                    return
                self.last_error = None
            except Exception as error:
                self.remaining()
                self.last_error = error
            time.sleep(min(self.interval, self.remaining()))
