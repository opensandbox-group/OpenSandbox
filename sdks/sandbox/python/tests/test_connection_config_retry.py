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
"""ConnectionConfig / ConnectionConfigSync integration with RetryPolicy."""

from __future__ import annotations

from datetime import timedelta

import httpx

from opensandbox.config import ConnectionConfig, ConnectionConfigSync
from opensandbox.transport import (
    RetryAsyncTransport,
    RetryPolicy,
    RetrySyncTransport,
)


class TestAsyncConfig:
    def test_default_policy_wraps_transport(self) -> None:
        cfg = ConnectionConfig().with_transport_if_missing()
        assert isinstance(cfg.transport, RetryAsyncTransport)

    def test_disabled_policy_skips_wrapping(self) -> None:
        cfg = ConnectionConfig(
            retry_policy=RetryPolicy.disabled()
        ).with_transport_if_missing()
        assert not isinstance(cfg.transport, RetryAsyncTransport)

    def test_no_retry_but_per_attempt_timeout_still_wraps(self) -> None:
        # max_retries=0 with per_attempt_timeout means the caller wants
        # a single, time-bounded attempt: the wrapper is still required
        # so the timeout gets enforced.
        cfg = ConnectionConfig(
            retry_policy=RetryPolicy(
                max_retries=0, per_attempt_timeout=timedelta(seconds=1)
            )
        ).with_transport_if_missing()
        assert isinstance(cfg.transport, RetryAsyncTransport)

    def test_no_retry_but_overall_deadline_still_wraps(self) -> None:
        cfg = ConnectionConfig(
            retry_policy=RetryPolicy(
                max_retries=0, overall_deadline=timedelta(seconds=1)
            )
        ).with_transport_if_missing()
        assert isinstance(cfg.transport, RetryAsyncTransport)

    def test_user_supplied_transport_unchanged(self) -> None:
        supplied = httpx.AsyncHTTPTransport()
        cfg = ConnectionConfig(transport=supplied)
        cfg = cfg.with_transport_if_missing()
        assert cfg.transport is supplied


class TestSyncConfig:
    def test_default_policy_wraps_transport(self) -> None:
        cfg = ConnectionConfigSync().with_transport_if_missing()
        assert isinstance(cfg.transport, RetrySyncTransport)

    def test_disabled_policy_skips_wrapping(self) -> None:
        cfg = ConnectionConfigSync(
            retry_policy=RetryPolicy.disabled()
        ).with_transport_if_missing()
        assert not isinstance(cfg.transport, RetrySyncTransport)

    def test_no_retry_but_per_attempt_timeout_still_wraps(self) -> None:
        cfg = ConnectionConfigSync(
            retry_policy=RetryPolicy(
                max_retries=0, per_attempt_timeout=timedelta(seconds=1)
            )
        ).with_transport_if_missing()
        assert isinstance(cfg.transport, RetrySyncTransport)

    def test_no_retry_but_overall_deadline_still_wraps(self) -> None:
        cfg = ConnectionConfigSync(
            retry_policy=RetryPolicy(
                max_retries=0, overall_deadline=timedelta(seconds=1)
            )
        ).with_transport_if_missing()
        assert isinstance(cfg.transport, RetrySyncTransport)

    def test_user_supplied_transport_unchanged(self) -> None:
        supplied = httpx.HTTPTransport()
        cfg = ConnectionConfigSync(transport=supplied)
        cfg = cfg.with_transport_if_missing()
        assert cfg.transport is supplied
