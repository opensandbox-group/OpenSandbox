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

"""Pause executor: the sweeper's single authority for issuing pauses.

Wraps ``FastPathClient.pause_sandbox`` with the OSEP-0024 pause contract:
idempotent ``idlepause-{sandbox_id}-{generation}`` request id (stable across
retries of one intent, distinct per idle episode so FastPath-side dedup can
never replay an earlier episode's outcome), plus the uid and
observed-generation fence. A fence rejection (``FailedPrecondition``) is
re-evaluated next sweep.
"""

from __future__ import annotations

import enum
import logging
from typing import Protocol

from opensandbox_server.integrations.idle_pause.constants import IDLE_PAUSE_REQUEST_ID_PREFIX
from opensandbox_server.integrations.idle_pause.inventory import SandboxCandidate
from opensandbox_server.services.fast_sandbox.fastpath_client import (
    FastPathClient,
    FastPathConflict,
    FastPathError,
    FastPathFailedPrecondition,
    FastPathNotFound,
    FastPathUnavailable,
)

logger = logging.getLogger(__name__)


class PauseOutcome(enum.Enum):
    ACCEPTED = "accepted"
    STATE_CHANGED = "state_changed"  # fence rejected; next sweep re-evaluates
    NOT_FOUND = "not_found"
    UNAVAILABLE = "unavailable"
    FAILED = "failed"


class PauseExecutor(Protocol):
    def pause(self, candidate: SandboxCandidate) -> PauseOutcome: ...


class FastPathPauseExecutor:
    def __init__(self, client: FastPathClient):
        self._client = client

    def pause(self, candidate: SandboxCandidate) -> PauseOutcome:
        # Unique per checkpoint intent: retries of one episode share the id
        # (idempotent replay), later episodes differ (dedup cannot replay a
        # rejected earlier attempt).
        request_id = f"{IDLE_PAUSE_REQUEST_ID_PREFIX}{candidate.sandbox_id}-{candidate.generation}"
        try:
            self._client.pause_sandbox(
                candidate.namespace,
                candidate.sandbox_id,
                expected_uid=candidate.uid,
                expected_generation=candidate.generation,
                request_id=request_id,
            )
        except (FastPathFailedPrecondition, FastPathConflict) as exc:
            logger.info(
                f"idle_pause pause fence rejected sandbox_id={candidate.sandbox_id} "
                f"code={exc.code}"
            )
            return PauseOutcome.STATE_CHANGED
        except FastPathNotFound:
            return PauseOutcome.NOT_FOUND
        except FastPathUnavailable as exc:
            logger.warning(
                f"idle_pause pause unavailable sandbox_id={candidate.sandbox_id} "
                f"code={exc.code}"
            )
            return PauseOutcome.UNAVAILABLE
        except FastPathError as exc:
            logger.warning(
                f"idle_pause pause failed sandbox_id={candidate.sandbox_id} "
                f"code={exc.code} message={exc.message}"
            )
            return PauseOutcome.FAILED
        return PauseOutcome.ACCEPTED


__all__ = ["FastPathPauseExecutor", "PauseExecutor", "PauseOutcome"]
