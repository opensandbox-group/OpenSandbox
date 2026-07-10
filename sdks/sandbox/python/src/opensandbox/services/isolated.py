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
"""
Isolated session service interface.

Protocol for namespace-isolated execution operations (OSEP-0013).
"""

import logging
from collections.abc import AsyncIterator
from contextlib import AbstractAsyncContextManager, asynccontextmanager
from typing import Protocol

from opensandbox.models.execd import Execution, ExecutionHandlers
from opensandbox.models.isolated import (
    BindMount,
    CreateIsolatedSessionRequest,
    IsolatedCapabilities,
    IsolatedRunOpts,
    IsolatedSessionInfo,
    IsolatedSessionState,
    IsolatedSessionSummary,
    IsolatedWorkspaceSpec,
)
from opensandbox.services.filesystem import Filesystem

logger = logging.getLogger(__name__)


class IsolationSession(Protocol):
    """Handle to a single isolated bash session."""

    @property
    def session_id(self) -> str: ...

    @property
    def info(self) -> IsolatedSessionInfo: ...

    @property
    def files(self) -> Filesystem: ...

    async def run(
        self,
        code: str,
        *,
        opts: IsolatedRunOpts | None = None,
        handlers: ExecutionHandlers | None = None,
    ) -> Execution: ...

    async def get(self) -> IsolatedSessionState: ...

    async def delete(self) -> None: ...


class IsolationService(Protocol):
    """Service for managing namespace-isolated bash sessions."""

    async def create(
        self, request: CreateIsolatedSessionRequest
    ) -> IsolationSession: ...

    async def capabilities(self) -> IsolatedCapabilities: ...

    async def list(self) -> list[IsolatedSessionSummary]: ...

    async def run_once(
        self,
        code: str,
        *,
        workspace: str,
        workspace_mode: str | None = None,
        opts: IsolatedRunOpts | None = None,
        handlers: ExecutionHandlers | None = None,
        profile: str | None = None,
        share_net: bool | None = None,
        binds: list[BindMount] | None = None,
    ) -> Execution:
        """Create a session, run *code*, and delete the session (auto-cleanup)."""
        ...

    def session(
        self,
        request: CreateIsolatedSessionRequest,
    ) -> AbstractAsyncContextManager[IsolationSession]:
        """Async context manager: create a session and delete it on exit."""
        ...


class IsolationServiceMixin:
    """Default implementations for :class:`IsolationService` convenience helpers.

    Concrete adapters provide ``create``/``capabilities`` and inherit
    ``run_once``/``session`` from this mixin (mirrors Kotlin's default
    interface methods). ``create`` is the only required dependency.
    """

    async def create(
        self, request: CreateIsolatedSessionRequest
    ) -> IsolationSession: ...

    async def run_once(
        self,
        code: str,
        *,
        workspace: str,
        workspace_mode: str | None = None,
        opts: IsolatedRunOpts | None = None,
        handlers: ExecutionHandlers | None = None,
        profile: str | None = None,
        share_net: bool | None = None,
        binds: list[BindMount] | None = None,
    ) -> Execution:
        request = CreateIsolatedSessionRequest(
            workspace=IsolatedWorkspaceSpec(path=workspace, mode=workspace_mode),
            profile=profile,
            share_net=share_net,
            binds=binds,
        )
        session = await self.create(request)
        try:
            return await session.run(code, opts=opts, handlers=handlers)
        finally:
            try:
                await session.delete()
            except Exception:
                logger.warning(
                    "failed to delete isolated session %s", session.session_id
                )

    @asynccontextmanager
    async def session(
        self,
        request: CreateIsolatedSessionRequest,
    ) -> AsyncIterator[IsolationSession]:
        session = await self.create(request)
        try:
            yield session
        finally:
            try:
                await session.delete()
            except Exception:
                logger.warning(
                    "failed to delete isolated session %s", session.session_id
                )
