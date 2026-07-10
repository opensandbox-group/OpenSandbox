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
Synchronous isolated session service interface.
"""

import logging
from collections.abc import Iterator
from contextlib import AbstractContextManager, contextmanager
from typing import Any, Protocol

from opensandbox.models.execd import Execution
from opensandbox.models.execd_sync import ExecutionHandlersSync
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

logger = logging.getLogger(__name__)


class IsolationSessionSync(Protocol):
    """Sync handle to a single isolated session."""

    @property
    def session_id(self) -> str: ...

    @property
    def info(self) -> IsolatedSessionInfo: ...

    @property
    def files(self) -> Any: ...

    def run(
        self,
        code: str,
        *,
        opts: IsolatedRunOpts | None = None,
        handlers: ExecutionHandlersSync | None = None,
    ) -> Execution: ...

    def get(self) -> IsolatedSessionState: ...

    def delete(self) -> None: ...


class IsolationServiceSync(Protocol):
    """Sync service for managing namespace-isolated bash sessions."""

    def create(
        self, request: CreateIsolatedSessionRequest
    ) -> IsolationSessionSync: ...

    def capabilities(self) -> IsolatedCapabilities: ...

    def list(self) -> list[IsolatedSessionSummary]: ...

    def run_once(
        self,
        code: str,
        *,
        workspace: str,
        workspace_mode: str | None = None,
        opts: IsolatedRunOpts | None = None,
        handlers: ExecutionHandlersSync | None = None,
        profile: str | None = None,
        share_net: bool | None = None,
        binds: list[BindMount] | None = None,
    ) -> Execution:
        """Create a session, run *code*, and delete the session (auto-cleanup)."""
        ...

    def session(
        self,
        request: CreateIsolatedSessionRequest,
    ) -> AbstractContextManager[IsolationSessionSync]:
        """Context manager: create a session and delete it on exit."""
        ...


class IsolationServiceSyncMixin:
    """Default implementations for :class:`IsolationServiceSync` helpers.

    Concrete adapters provide ``create``/``capabilities`` and inherit
    ``run_once``/``session`` from this mixin (mirrors Kotlin's default
    interface methods).
    """

    def create(
        self, request: CreateIsolatedSessionRequest
    ) -> IsolationSessionSync: ...

    def run_once(
        self,
        code: str,
        *,
        workspace: str,
        workspace_mode: str | None = None,
        opts: IsolatedRunOpts | None = None,
        handlers: ExecutionHandlersSync | None = None,
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
        session = self.create(request)
        try:
            return session.run(code, opts=opts, handlers=handlers)
        finally:
            try:
                session.delete()
            except Exception:
                logger.warning(
                    "failed to delete isolated session %s", session.session_id
                )

    @contextmanager
    def session(
        self,
        request: CreateIsolatedSessionRequest,
    ) -> Iterator[IsolationSessionSync]:
        session = self.create(request)
        try:
            yield session
        finally:
            try:
                session.delete()
            except Exception:
                logger.warning(
                    "failed to delete isolated session %s", session.session_id
                )
