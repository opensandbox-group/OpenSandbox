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
"""Unit tests for ReadinessBudget error attribution.

Regression coverage for an audit finding: health() / health_sync() used to
raise the timeout exception *before* recording the error that triggered it, so
the reported cause was the previous iteration's error (or none at all on the
first iteration). The latest real error must be what surfaces.
"""

from __future__ import annotations

from datetime import timedelta

import pytest

from opensandbox.exceptions import SandboxReadyTimeoutException
from opensandbox.internal.readiness import ReadinessBudget


def _short_budget() -> ReadinessBudget:
    return ReadinessBudget(
        timeout=timedelta(milliseconds=50), interval=timedelta(milliseconds=5)
    )


@pytest.mark.asyncio
async def test_async_health_reports_latest_error_on_timeout() -> None:
    budget = _short_budget()
    errors: list[str] = []

    async def action() -> bool:
        error = f"failure {budget.attempts}"
        errors.append(error)
        raise RuntimeError(error)

    with pytest.raises(SandboxReadyTimeoutException) as caught:
        await budget.health(action, context="test")

    assert len(errors) >= 1
    assert str(budget.last_error) == errors[-1], (
        "health() must report the latest error, not the previous iteration's"
    )
    assert str(caught.value.__cause__) == errors[-1]


def test_sync_health_reports_latest_error_on_timeout() -> None:
    budget = _short_budget()
    errors: list[str] = []

    def action() -> bool:
        error = f"failure {budget.attempts}"
        errors.append(error)
        raise RuntimeError(error)

    with pytest.raises(SandboxReadyTimeoutException) as caught:
        budget.health_sync(action, context="test")

    assert len(errors) >= 1
    assert str(budget.last_error) == errors[-1], (
        "health_sync() must report the latest error, not the previous iteration's"
    )
    assert str(caught.value.__cause__) == errors[-1]


def test_sync_run_records_latest_error_not_first() -> None:
    budget = _short_budget()
    errors: list[str] = []

    def action() -> None:
        errors.append(f"error {len(errors) + 1}")
        raise RuntimeError(errors[-1])

    with pytest.raises(SandboxReadyTimeoutException):
        while True:
            try:
                budget.run_sync(action)
            except SandboxReadyTimeoutException:
                raise
            except RuntimeError:
                # Keep hammering until the budget expires.
                continue

    assert str(budget.last_error) == errors[-1], (
        "run_sync must record the latest error, not the first one"
    )
