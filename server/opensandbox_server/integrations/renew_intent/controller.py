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

"""Renew access path: eligibility checks then ``renew_expiration``."""

from __future__ import annotations

import asyncio
import logging
from datetime import datetime, timedelta, timezone
from typing import TYPE_CHECKING

from fastapi import HTTPException

from opensandbox_server.api.schema import RenewSandboxExpirationRequest
from opensandbox_server.integrations.renew_intent.intent import RenewIntent
from opensandbox_server.integrations.renew_intent.logutil import (
    RENEW_EVENT_FAILED,
    RENEW_EVENT_SUCCEEDED,
    RENEW_SOURCE_REDIS_QUEUE,
    RENEW_SOURCE_SERVER_PROXY,
    renew_bundle,
)

if TYPE_CHECKING:
    from opensandbox_server.services.extension_service import ExtensionService
    from opensandbox_server.services.sandbox_service import SandboxService

logger = logging.getLogger(__name__)


def _http_detail_str(detail: object) -> str:
    if isinstance(detail, dict):
        return str(detail.get("message", detail))
    return str(detail)


class AccessRenewController:
    """Eligibility gates and ``renew_expiration``; rate limiting is expected upstream (ingress / proxy)."""

    def __init__(
        self,
        sandbox_service: "SandboxService",
        extension_service: "ExtensionService",
    ) -> None:
        self._sandbox_service = sandbox_service
        self._extension_service = extension_service

    def _try_renew_sync(self, sandbox_id: str, *, source: str) -> bool:
        try:
            sandbox = self._sandbox_service.get_sandbox(sandbox_id)
        except HTTPException:
            return False

        if sandbox.status.state.lower() != "running":
            return False

        if sandbox.expires_at is None:
            return False

        extend = self._extension_service.get_access_renew_extend_seconds(sandbox_id)
        if extend is None:
            return False

        now = datetime.now(timezone.utc)
        current = sandbox.expires_at
        if current.tzinfo is None:
            current = current.replace(tzinfo=timezone.utc)

        candidate = now + timedelta(seconds=extend)
        new_expires = max(candidate, current)

        req = RenewSandboxExpirationRequest(expires_at=new_expires)
        try:
            self._sandbox_service.renew_expiration(sandbox_id, req)
        except HTTPException as exc:
            detail_s = _http_detail_str(exc.detail)
            line, ex = renew_bundle(
                event=RENEW_EVENT_FAILED,
                source=source,
                sandbox_id=sandbox_id,
                skip_reason="renew_expiration_rejected",
                http_detail=detail_s,
                http_status=getattr(exc, "status_code", None),
            )
            logger.warning(f"renew_intent {line} detail={detail_s}", extra=ex)
            return False
        except Exception as exc:
            line, ex = renew_bundle(
                event=RENEW_EVENT_FAILED,
                source=source,
                sandbox_id=sandbox_id,
                skip_reason="renew_expiration_error",
                error_type=type(exc).__name__,
            )
            logger.exception(f"renew_intent {line}", extra=ex)
            return False

        new_expires_iso = new_expires.isoformat()
        line, ex = renew_bundle(
            event=RENEW_EVENT_SUCCEEDED,
            source=source,
            sandbox_id=sandbox_id,
            new_expires_at=new_expires_iso,
        )
        logger.info(f"renew_intent {line}", extra=ex)
        return True

    def attempt_renew_sync(self, sandbox_id: str, *, source: str = RENEW_SOURCE_SERVER_PROXY) -> bool:
        """Run gates + renew (sync)."""
        return self._try_renew_sync(sandbox_id, source=source)

    async def renew_after_gates(self, sandbox_id: str, *, source: str) -> None:
        """Run renew in a worker thread (caller holds per-sandbox serialization)."""
        await asyncio.to_thread(self._try_renew_sync, sandbox_id, source=source)

    async def process_intent_after_lock(self, intent: RenewIntent) -> None:
        await self.renew_after_gates(intent.sandbox_id, source=RENEW_SOURCE_REDIS_QUEUE)
