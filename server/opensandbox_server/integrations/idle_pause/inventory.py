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

"""Candidate inventory: the enumeration source for the sweeper.

The sandbox list — never a Redis key scan — drives enumeration (a sandbox
with a missing activity key is exactly the invisible case). The fsb adapter
reads informer-cached Sandbox CRs, which carry the idle-policy annotation and
``spec.expireTime`` that FastPath v2 summaries lack.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from datetime import datetime
from typing import Optional, Protocol

from opensandbox_server.extensions.keys import IDLE_POLICY_ANNOTATION_KEY
from opensandbox_server.integrations.idle_pause.policy import (
    IdlePolicy,
    idle_policy_from_annotation,
)
from opensandbox_server.services.fast_sandbox.cr_reader import SandboxCRReader
from opensandbox_server.services.fast_sandbox.cr_mapping import sandbox_from_cr

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class SandboxCandidate:
    """Everything the sweeper's gates need about one sandbox."""

    sandbox_id: str
    namespace: str
    uid: str
    generation: int
    state: str  # public lifecycle state, e.g. "Running" / "Paused" / "Failed"
    expires_at: Optional[datetime]
    policy: Optional[IdlePolicy]


class SandboxInventory(Protocol):
    """Snapshot of sandbox facts for one namespace (synchronous, cache-backed)."""

    def snapshot(self, namespace: str) -> list[SandboxCandidate]:
        """Return every sandbox visible in the namespace (unfiltered)."""
        ...


class FastSandboxInventory:
    """``SandboxInventory`` backed by the fsb CR reader (fsb- sandboxes only)."""

    def __init__(self, cr_reader: SandboxCRReader):
        self._cr_reader = cr_reader

    def snapshot(self, namespace: str) -> list[SandboxCandidate]:
        candidates: list[SandboxCandidate] = []
        try:
            crs = self._cr_reader.list(namespace)
        except Exception as exc:
            # Fail toward "not pausing": sweep this namespace as empty.
            logger.warning(f"idle_pause inventory read failed namespace={namespace} error={exc}")
            return []
        for cr in crs:
            candidate = _candidate_from_cr(cr, namespace)
            if candidate is not None:
                candidates.append(candidate)
        return candidates


def _candidate_from_cr(cr: dict, namespace: str) -> Optional[SandboxCandidate]:
    metadata = cr.get("metadata") or {}
    sandbox_id = metadata.get("name")
    if not sandbox_id:
        return None
    uid = metadata.get("uid") or ""
    generation = int(metadata.get("generation") or 0)
    try:
        sandbox = sandbox_from_cr(cr)
        state = sandbox.status.state
    except Exception as exc:
        # Unmappable (e.g. corrupt expireTime): the expiry gate cannot be
        # verified, so the candidate must be dropped.
        logger.warning(f"idle_pause candidate mapping failed sandbox_id={sandbox_id} error={exc}")
        return None

    annotations = metadata.get("annotations") or {}
    policy = idle_policy_from_annotation(annotations.get(IDLE_POLICY_ANNOTATION_KEY))

    return SandboxCandidate(
        sandbox_id=sandbox_id,
        namespace=namespace,
        uid=uid,
        generation=generation,
        state=state,
        expires_at=sandbox.expires_at,
        policy=policy,
    )
