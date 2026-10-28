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

"""Activity-probe execution (OSEP-0024).

The sweeper asks a sandbox that declares ``idlePolicy.activeProbe`` whether
work is in progress: an ordinary GET through the ingress gateway, carrying
the ``OpenSandbox-Skip-Activity`` sentinel so the probe never resets the
clock it measures. Fail-safe mapping: a busy answer records activity, an
explicit not-active lets the pause proceed, everything else drops the sweep.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import Callable, Optional

import httpx

from opensandbox_server.api.schema import Endpoint
from opensandbox_server.integrations.idle_pause.inventory import SandboxCandidate

logger = logging.getLogger(__name__)

# Code-level convention shared with the ingress recorder (not a config knob):
# the ingress suppresses activity tracking for the exact sentinel value.
SKIP_ACTIVITY_HEADER = "OpenSandbox-Skip-Activity"
SKIP_ACTIVITY_VALUE = "skip"

PROBE_OUTCOME_ACTIVE = "active"
PROBE_OUTCOME_NOT_ACTIVE = "not_active"
PROBE_OUTCOME_ERROR = "error"

# Cap on the application-supplied ``message`` carried into the audit line.
MAX_PROBE_MESSAGE_CHARS = 256


@dataclass(frozen=True)
class ProbeVerdict:
    outcome: str
    message: Optional[str] = None

    def audit_note(self) -> str:
        """One-line audit fragment for the structured pause log."""
        if self.message:
            return f"probe=not_active message={self.message!r}"
        return "probe=not_active"


def _probe_target_url(endpoint: Endpoint, path: str) -> str:
    """Join the gateway endpoint and probe path into an absolute URL.

    ``ingress.gateway.address`` is configured without a scheme; default to
    plain http, the same way the server proxy path targets the gateway
    (route authenticity comes from the signed scope, not TLS).
    """
    base = endpoint.endpoint.rstrip("/")
    if not base.startswith(("http://", "https://")):
        base = f"http://{base}"
    return base + path


class ActivityProber:
    """Issues the app-declared activity probe through the ingress gateway."""

    def __init__(
        self,
        *,
        endpoint_builder: Callable[[str, str, int], Endpoint],
        client: Optional[httpx.AsyncClient] = None,
    ) -> None:
        self._endpoint_builder = endpoint_builder
        self._client = client or httpx.AsyncClient()

    async def probe(self, candidate: SandboxCandidate) -> ProbeVerdict:
        """Probe one candidate; never raises (every failure is a verdict)."""
        policy = candidate.policy
        assert policy is not None and policy.active_probe is not None
        probe = policy.active_probe
        try:
            endpoint = self._endpoint_builder(
                candidate.namespace, candidate.sandbox_id, probe.port
            )
            url = _probe_target_url(endpoint, probe.path)
            headers = dict(endpoint.headers or {})
            headers[SKIP_ACTIVITY_HEADER] = SKIP_ACTIVITY_VALUE
            response = await self._client.get(
                url, headers=headers, timeout=probe.timeout_seconds
            )
        except Exception as exc:
            logger.debug(
                f"idle_pause probe request failed sandbox_id={candidate.sandbox_id} "
                f"error={exc}"
            )
            return ProbeVerdict(PROBE_OUTCOME_ERROR)

        if response.status_code != 200:
            return ProbeVerdict(PROBE_OUTCOME_ERROR)
        try:
            payload = response.json()
        except ValueError:
            return ProbeVerdict(PROBE_OUTCOME_ERROR)
        if not isinstance(payload, dict):
            return ProbeVerdict(PROBE_OUTCOME_ERROR)
        active = payload.get("active")
        if not isinstance(active, bool):
            return ProbeVerdict(PROBE_OUTCOME_ERROR)
        if active:
            return ProbeVerdict(PROBE_OUTCOME_ACTIVE)
        message = payload.get("message")
        if isinstance(message, str) and message:
            return ProbeVerdict(PROBE_OUTCOME_NOT_ACTIVE, message[:MAX_PROBE_MESSAGE_CHARS])
        return ProbeVerdict(PROBE_OUTCOME_NOT_ACTIVE)

    async def aclose(self) -> None:
        if self._client is not None:
            await self._client.aclose()


__all__ = [
    "MAX_PROBE_MESSAGE_CHARS",
    "SKIP_ACTIVITY_HEADER",
    "SKIP_ACTIVITY_VALUE",
    "ActivityProber",
    "ProbeVerdict",
    "PROBE_OUTCOME_ACTIVE",
    "PROBE_OUTCOME_ERROR",
    "PROBE_OUTCOME_NOT_ACTIVE",
]
