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

"""SDK metrics ingestion endpoints."""

from __future__ import annotations

import logging
import re
from typing import Optional

from fastapi import APIRouter, Header, Response, status

from opensandbox_server.api.schema import MetricsEvent
from opensandbox_server.integrations.otel import record_sandbox_create_duration

logger = logging.getLogger(__name__)

router = APIRouter(tags=["Metrics"])

# OpenSandbox-Python-SDK/0.1.14, OpenSandbox-Go-SDK/0.1.0, OpenSandbox-JS-SDK/0.1.10
_SDK_USER_AGENT_RE = re.compile(
    r"OpenSandbox-([A-Za-z0-9]+)-SDK/([^\s]+)",
    re.IGNORECASE,
)


def parse_sdk_user_agent(user_agent: Optional[str]) -> tuple[str, str]:
    """Extract (language, version) from SDK User-Agent; fallback to unknown."""
    if not user_agent:
        return "unknown", "unknown"
    match = _SDK_USER_AGENT_RE.search(user_agent)
    if not match:
        return "unknown", "unknown"
    return match.group(1).lower(), match.group(2)


@router.post(
    "/metrics/events",
    status_code=status.HTTP_204_NO_CONTENT,
    responses={
        400: {"description": "Bad request"},
        401: {"description": "Unauthorized"},
        500: {"description": "Internal server error"},
    },
)
def report_metrics_event(
    event: MetricsEvent,
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID"),
    user_agent: Optional[str] = Header(None, alias="User-Agent"),
) -> Response:
    """Accept best-effort SDK metrics and record to OTEL (noop when disabled)."""
    _ = x_request_id
    if event.event_type == "sandbox.create":
        sdk_language, sdk_version = parse_sdk_user_agent(user_agent)
        record_sandbox_create_duration(
            create_duration_ms=event.create_duration_ms,
            sdk_language=sdk_language,
            sdk_version=sdk_version,
            success=event.success,
        )
        logger.debug(
            "Accepted sandbox.create metrics event sandbox_id=%s image=%s "
            "duration_ms=%s sdk=%s/%s success=%s",
            event.sandbox_id,
            event.image,
            event.create_duration_ms,
            sdk_language,
            sdk_version,
            event.success,
        )
    return Response(status_code=status.HTTP_204_NO_CONTENT)
