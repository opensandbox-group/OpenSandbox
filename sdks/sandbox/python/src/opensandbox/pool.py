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
"""Sandbox pool public exports."""

from opensandbox._async_pool_store import InMemoryAsyncPoolStateStore
from opensandbox._pool_store import InMemoryPoolStateStore
from opensandbox.pool_async import AsyncSandboxPool, SandboxPoolAsync
from opensandbox.pool_manager import (
    SandboxPoolManager,
    SandboxPoolManagerAsync,
    SandboxPoolManagerSync,
)
from opensandbox.pool_types import (
    AcquirePolicy,
    AsyncPoolConfig,
    AsyncPooledSandboxCreator,
    AsyncPoolStateStore,
    IdleEntry,
    PoolConfig,
    PoolCreationSpec,
    PoolDestroyOptions,
    PoolDestroyResult,
    PoolDestroyState,
    PoolDestroyStrategy,
    PooledSandboxCreateContext,
    PooledSandboxCreateReason,
    PooledSandboxCreator,
    PoolLifecycleState,
    PoolSnapshot,
    PoolState,
    PoolStateStore,
    StoreCounters,
    TakeIdleResult,
)
from opensandbox.sync.pool import SandboxPoolSync

SandboxPool = SandboxPoolSync

__all__ = [
    "AcquirePolicy",
    "AsyncPooledSandboxCreator",
    "AsyncPoolConfig",
    "AsyncPoolStateStore",
    "AsyncSandboxPool",
    "IdleEntry",
    "InMemoryAsyncPoolStateStore",
    "InMemoryPoolStateStore",
    "PooledSandboxCreateContext",
    "PooledSandboxCreateReason",
    "PooledSandboxCreator",
    "PoolConfig",
    "PoolCreationSpec",
    "PoolDestroyOptions",
    "PoolDestroyResult",
    "PoolDestroyState",
    "PoolDestroyStrategy",
    "PoolLifecycleState",
    "PoolSnapshot",
    "PoolState",
    "PoolStateStore",
    "SandboxPoolAsync",
    "SandboxPool",
    "SandboxPoolManager",
    "SandboxPoolManagerAsync",
    "SandboxPoolManagerSync",
    "SandboxPoolSync",
    "StoreCounters",
    "TakeIdleResult",
]
