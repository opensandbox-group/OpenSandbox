#
# Copyright 2025 Alibaba Group Holding Ltd.
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
"""Tests for ``PoolConfig`` and ``AsyncPoolConfig`` validation."""

from __future__ import annotations

import inspect
from datetime import timedelta

import pytest

from opensandbox.config.connection import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.pool import (
    AsyncPoolConfig,
    InMemoryAsyncPoolStateStore,
    InMemoryPoolStateStore,
    PoolConfig,
    PoolCreationSpec,
    PooledSandboxCreateContext,
    SandboxPoolAsync,
)
from opensandbox.sandbox import Sandbox
from opensandbox.sync.pool import SandboxPoolSync
from opensandbox.sync.sandbox import SandboxSync


def _sync_kwargs() -> dict[str, object]:
    return {
        "pool_name": "test",
        "max_idle": 1,
        "state_store": InMemoryPoolStateStore(),
        "connection_config": ConnectionConfigSync(),
        "creation_spec": PoolCreationSpec(image="ubuntu:22.04"),
    }


def _async_kwargs() -> dict[str, object]:
    return {
        "pool_name": "test",
        "max_idle": 1,
        "state_store": InMemoryAsyncPoolStateStore(),
        "connection_config": ConnectionConfig(),
        "creation_spec": PoolCreationSpec(image="ubuntu:22.04"),
    }


def test_default_acquire_min_remaining_ttl_is_60s_for_long_idle_timeout() -> None:
    # 24h idle_timeout caps at 60s.
    config = PoolConfig(**_sync_kwargs())  # type: ignore[arg-type]
    assert config.acquire_min_remaining_ttl == timedelta(seconds=60)


def test_async_default_acquire_min_remaining_ttl_is_60s_for_long_idle_timeout() -> None:
    config = AsyncPoolConfig(**_async_kwargs())  # type: ignore[arg-type]
    assert config.acquire_min_remaining_ttl == timedelta(seconds=60)


def test_default_acquire_min_remaining_ttl_scales_for_short_idle_timeout() -> None:
    # idle_timeout=30s ⇒ default = min(60s, 30s/2) = 15s. Existing users with short
    # idle timeouts must not get a config-time error from a hidden 60s default.
    config = PoolConfig(**_sync_kwargs(), idle_timeout=timedelta(seconds=30))  # type: ignore[arg-type]
    assert config.acquire_min_remaining_ttl == timedelta(seconds=15)


def test_negative_acquire_min_remaining_ttl_rejected() -> None:
    with pytest.raises(
        ValueError, match="acquire_min_remaining_ttl must be non-negative"
    ):
        PoolConfig(**_sync_kwargs(), acquire_min_remaining_ttl=timedelta(seconds=-1))  # type: ignore[arg-type]


def test_explicit_acquire_min_remaining_ttl_at_or_above_idle_timeout_rejected() -> None:
    # The auto-default protects against this, but an explicit value above idle_timeout
    # still fails validation.
    with pytest.raises(ValueError, match="strictly less than"):
        PoolConfig(  # type: ignore[arg-type]
            **_sync_kwargs(),
            idle_timeout=timedelta(seconds=30),
            acquire_min_remaining_ttl=timedelta(seconds=30),
        )


def test_async_explicit_acquire_min_remaining_ttl_at_or_above_idle_timeout_rejected() -> (
    None
):
    with pytest.raises(ValueError, match="strictly less than"):
        AsyncPoolConfig(  # type: ignore[arg-type]
            **_async_kwargs(),
            idle_timeout=timedelta(seconds=30),
            acquire_min_remaining_ttl=timedelta(seconds=30),
        )


def test_acquire_min_remaining_ttl_just_below_idle_timeout_accepted() -> None:
    config = PoolConfig(
        **_sync_kwargs(),  # type: ignore[arg-type]
        idle_timeout=timedelta(seconds=10),
        acquire_min_remaining_ttl=timedelta(seconds=9),
    )
    assert config.acquire_min_remaining_ttl == timedelta(seconds=9)
    assert config.idle_timeout == timedelta(seconds=10)


def test_zero_acquire_min_remaining_ttl_opts_out() -> None:
    # Explicit Duration.ZERO ↔ legacy binary-expiry behavior; valid configuration.
    config = PoolConfig(
        **_sync_kwargs(),  # type: ignore[arg-type]
        acquire_min_remaining_ttl=timedelta(0),
    )
    assert config.acquire_min_remaining_ttl == timedelta(0)


def test_sync_pool_config_positional_owner_id_stays_compatible() -> None:
    kwargs = _sync_kwargs()

    config = PoolConfig(
        kwargs["pool_name"],  # type: ignore[arg-type]
        kwargs["max_idle"],  # type: ignore[arg-type]
        kwargs["state_store"],  # type: ignore[arg-type]
        kwargs["connection_config"],  # type: ignore[arg-type]
        kwargs["creation_spec"],  # type: ignore[arg-type]
        "owner-1",
    )

    assert config.owner_id == "owner-1"
    assert config.sandbox_creator is None


def test_async_pool_config_positional_owner_id_stays_compatible() -> None:
    kwargs = _async_kwargs()

    config = AsyncPoolConfig(
        kwargs["pool_name"],  # type: ignore[arg-type]
        kwargs["max_idle"],  # type: ignore[arg-type]
        kwargs["state_store"],  # type: ignore[arg-type]
        kwargs["connection_config"],  # type: ignore[arg-type]
        kwargs["creation_spec"],  # type: ignore[arg-type]
        "owner-1",
    )

    assert config.owner_id == "owner-1"
    assert config.sandbox_creator is None


def test_pool_facade_sandbox_creator_is_appended_after_factories() -> None:
    sync_params = list(inspect.signature(SandboxPoolSync).parameters)
    async_params = list(inspect.signature(SandboxPoolAsync).parameters)

    assert sync_params[-3:] == [
        "sandbox_manager_factory",
        "sandbox_factory",
        "sandbox_creator",
    ]
    assert async_params[-3:] == [
        "sandbox_manager_factory",
        "sandbox_factory",
        "sandbox_creator",
    ]


def test_sync_pool_config_keeps_sandbox_creator() -> None:
    def creator(_context: PooledSandboxCreateContext) -> SandboxSync:
        raise AssertionError("not called")

    config = PoolConfig(
        **_sync_kwargs(),  # type: ignore[arg-type]
        sandbox_creator=creator,
    )

    assert config.sandbox_creator is creator


def test_async_pool_config_keeps_sandbox_creator() -> None:
    async def creator(_context: PooledSandboxCreateContext) -> Sandbox:
        raise AssertionError("not called")

    config = AsyncPoolConfig(
        **_async_kwargs(),  # type: ignore[arg-type]
        sandbox_creator=creator,
    )

    assert config.sandbox_creator is creator


def test_sync_pool_facade_forwards_acquire_min_remaining_ttl() -> None:
    """``SandboxPoolSync.__init__`` exposes the new threshold and forwards it to the config.

    Without the constructor kwarg, users with ``idle_timeout <= 60s`` hit the hidden
    default and get a hard validation error with no way to override.
    """
    from opensandbox.sync.pool import SandboxPoolSync

    pool = SandboxPoolSync(
        pool_name="test",
        max_idle=1,
        state_store=InMemoryPoolStateStore(),
        connection_config=ConnectionConfigSync(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        idle_timeout=timedelta(seconds=30),
        acquire_min_remaining_ttl=timedelta(seconds=10),
    )

    assert pool._config.acquire_min_remaining_ttl == timedelta(seconds=10)
    assert pool._config.idle_timeout == timedelta(seconds=30)


def test_async_pool_facade_forwards_acquire_min_remaining_ttl() -> None:
    """``SandboxPoolAsync.__init__`` exposes the new threshold and forwards it to the config."""
    from opensandbox.pool_async import SandboxPoolAsync

    pool = SandboxPoolAsync(
        pool_name="test",
        max_idle=1,
        state_store=InMemoryAsyncPoolStateStore(),
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        idle_timeout=timedelta(seconds=30),
        acquire_min_remaining_ttl=timedelta(seconds=10),
    )

    assert pool._config.acquire_min_remaining_ttl == timedelta(seconds=10)
    assert pool._config.idle_timeout == timedelta(seconds=30)
