# pyright: reportAttributeAccessIssue=false
# protobuf-generated modules expose dynamic attributes.

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

"""Map fast-sandbox Sandbox status into the OpenSandbox Sandbox model.

fast-sandbox splits RuntimeReady (runtime up) from DataPlaneReady (routes and
Infra Components published). OpenSandbox reports Running only when both are
Ready, matching the "endpoint usable" expectation.

On expiry the reconciler keeps the Sandbox CRD with runtimeState=Stopped and a
RuntimeReady=False, reason=Expired Condition; that retained object maps to
Terminated. An actually missing CRD surfaces as gRPC NotFound at the client
layer and maps to HTTP 404 (no synthetic Sandbox object).
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Optional

from opensandbox_server.api.schema import ImageSpec, Sandbox, SandboxStatus
from opensandbox_server.services.fleets.create_mapping import (
    RENEW_EXTEND_SECONDS_METADATA_KEY,
)
from opensandbox_server.services.fleets.generated import (
    fastpath_pb2 as pb2,
)

#: FastPath metadata keys owned by the fleets backend, hidden from public reads.
RESERVED_METADATA_KEYS = frozenset({RENEW_EXTEND_SECONDS_METADATA_KEY})


def map_state(info: pb2.SandboxInfo) -> str:
    """Map a fast-sandbox SandboxInfo to the OpenSandbox lifecycle state."""
    runtime_state = info.runtime_state or ""
    data_plane_state = info.data_plane_state or ""

    if runtime_state == "Ready":
        if data_plane_state == "Ready":
            return "Running"
        # Runtime is up but routes/Infra are not published yet.
        return "Pending"
    if runtime_state in ("Pending", "Creating"):
        return "Pending"
    if runtime_state == "Draining":
        return "Stopping"
    if runtime_state == "Stopped":
        return "Terminated"
    if runtime_state in ("Failed", "Unavailable"):
        return "Failed"
    return "Pending"


def map_reason(info: pb2.SandboxInfo) -> Optional[str]:
    """Best-effort machine-readable reason for the mapped state.

    FastPath v2 SandboxInfo does not carry Conditions, so an Expired reason
    cannot be confirmed for a retained Stopped object; the reason is left
    unset rather than inventing a termination cause. Only states that are
    self-describing (Failed) report a reason.
    """
    if info.runtime_state == "Failed":
        return "Failed"
    return None


def map_sandbox(info: pb2.SandboxInfo) -> Sandbox:
    """Build the public Sandbox model from a FastPath SandboxInfo."""
    metadata = None
    if info.metadata:
        metadata = {
            key: value
            for key, value in info.metadata.items()
            if key not in RESERVED_METADATA_KEYS
        }
        if not metadata:
            metadata = None

    return Sandbox(
        id=info.sandbox_name,
        image=ImageSpec(uri=info.image) if info.image else None,
        status=SandboxStatus(state=map_state(info), reason=map_reason(info)),
        metadata=metadata,
        expiresAt=_to_datetime(info.expires_at_unix_seconds),
        createdAt=_to_datetime(info.created_at_unix_seconds),
    )


def _to_datetime(unix_seconds: int) -> Optional[datetime]:
    if unix_seconds <= 0:
        return None
    return datetime.fromtimestamp(unix_seconds, tz=timezone.utc)
