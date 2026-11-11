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

"""OSEP-0024 idle policy: model, validation, and annotation codec.

Persisted as the reserved Sandbox CR annotation ``sandbox.opensandbox.io/idle-policy``
(JSON), the same value the ingress wake gate parses. The server validates on
every write; consumers treat anything missing/unreadable/out-of-range as no
policy (never an implicit opt-in). The policy is desired state.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any, Optional

from opensandbox_server.extensions.keys import IDLE_POLICY_ANNOTATION_KEY
from opensandbox_server.integrations.idle_pause.constants import (
    DEFAULT_PROBE_TIMEOUT_SECONDS,
    IDLE_TIMEOUT_MAX_SECONDS,
    IDLE_TIMEOUT_MIN_SECONDS,
    ON_IDLE_ACTION_PAUSE,
    PROBE_TIMEOUT_MAX_SECONDS,
    PROBE_TIMEOUT_MIN_SECONDS,
)

__all__ = [
    "ActivityProbe",
    "IdlePolicy",
    "IDLE_POLICY_ANNOTATION_KEY",
    "idle_policy_from_annotation",
    "idle_policy_to_annotation",
    "validate_idle_policy",
]


@dataclass(frozen=True)
class ActivityProbe:
    """Application-declared activity probe (``idlePolicy.activeProbe``)."""

    port: int
    path: str
    timeout_seconds: int = DEFAULT_PROBE_TIMEOUT_SECONDS


@dataclass(frozen=True)
class IdlePolicy:
    """``idle_timeout_seconds is None`` models the wake-only policy."""

    idle_timeout_seconds: Optional[int] = None
    on_idle: Optional[str] = None
    wake_on_access: bool = True
    active_probe: Optional[ActivityProbe] = None

    @property
    def auto_pause_enabled(self) -> bool:
        return self.idle_timeout_seconds is not None


def validate_idle_policy(
    *,
    idle_timeout_seconds: Optional[int] = None,
    on_idle: Optional[str] = None,
    wake_on_access: bool = True,
    active_probe: Optional[ActivityProbe] = None,
    activity_ttl_seconds: Optional[int] = None,
) -> IdlePolicy:
    """Validate a create/update-time policy; raises ValueError (API 400).

    ``X <= activity_ttl_seconds`` keeps "activity key missing" always
    implying "idle for at least X".
    """
    if idle_timeout_seconds is None:
        if on_idle is not None:
            raise ValueError(
                "idlePolicy.onIdle must be omitted when idleTimeoutSeconds is omitted "
                "(a policy without a timeout is wake-only)."
            )
    else:
        if not isinstance(idle_timeout_seconds, int) or isinstance(idle_timeout_seconds, bool):
            raise ValueError("idlePolicy.idleTimeoutSeconds must be an integer.")
        if not IDLE_TIMEOUT_MIN_SECONDS <= idle_timeout_seconds <= IDLE_TIMEOUT_MAX_SECONDS:
            raise ValueError(
                f"idlePolicy.idleTimeoutSeconds must be between "
                f"{IDLE_TIMEOUT_MIN_SECONDS} and {IDLE_TIMEOUT_MAX_SECONDS} seconds."
            )
        if activity_ttl_seconds is not None and idle_timeout_seconds > activity_ttl_seconds:
            raise ValueError(
                f"idlePolicy.idleTimeoutSeconds ({idle_timeout_seconds}) must not exceed "
                f"the server's activity_ttl_seconds ({activity_ttl_seconds})."
            )
        if on_idle != ON_IDLE_ACTION_PAUSE:
            raise ValueError(
                f"idlePolicy.onIdle must be '{ON_IDLE_ACTION_PAUSE}' when "
                "idleTimeoutSeconds is set."
            )

    if not isinstance(wake_on_access, bool):
        # A non-bool would survive the write and void the whole declaration on read.
        raise ValueError("idlePolicy.wakeOnAccess must be a boolean.")

    if active_probe is not None:
        _validate_probe(active_probe)

    return IdlePolicy(
        idle_timeout_seconds=idle_timeout_seconds,
        on_idle=on_idle if idle_timeout_seconds is not None else None,
        wake_on_access=wake_on_access,
        active_probe=active_probe,
    )


def _probe_path_is_valid(path: object) -> bool:
    """Shared bounds for the create-side validator and the annotation decoder.

    The annotation can bypass the create path (kubectl annotate); the probe
    executor must never see a CRLF-laden path.
    """
    return (
        isinstance(path, str)
        and path.startswith("/")
        and len(path) <= 512
        and not any(ord(c) < 0x20 or c == "\x7f" or c.isspace() for c in path)
    )


def _validate_probe(probe: ActivityProbe) -> None:
    if not isinstance(probe.port, int) or isinstance(probe.port, bool) or not (
        1 <= probe.port <= 65535
    ):
        raise ValueError("idlePolicy.activeProbe.port must be an integer between 1 and 65535.")
    if not _probe_path_is_valid(probe.path):
        raise ValueError(
            "idlePolicy.activeProbe.path must be an absolute HTTP path (max 512 chars, "
            "no whitespace or control characters)."
        )
    if not isinstance(probe.timeout_seconds, int) or isinstance(probe.timeout_seconds, bool) or not (
        PROBE_TIMEOUT_MIN_SECONDS <= probe.timeout_seconds <= PROBE_TIMEOUT_MAX_SECONDS
    ):
        raise ValueError(
            f"idlePolicy.activeProbe.timeoutSeconds must be between "
            f"{PROBE_TIMEOUT_MIN_SECONDS} and {PROBE_TIMEOUT_MAX_SECONDS} seconds."
        )


def idle_policy_to_annotation(policy: IdlePolicy) -> str:
    """Canonical JSON (camelCase), shape-mirroring the ingress parser."""
    payload: dict[str, Any] = {}
    if policy.idle_timeout_seconds is not None:
        payload["idleTimeoutSeconds"] = policy.idle_timeout_seconds
        payload["onIdle"] = policy.on_idle or ON_IDLE_ACTION_PAUSE
    payload["wakeOnAccess"] = policy.wake_on_access
    if policy.active_probe is not None:
        probe = policy.active_probe
        payload["activeProbe"] = {
            "port": probe.port,
            "path": probe.path,
            "timeoutSeconds": probe.timeout_seconds,
        }
    return json.dumps(payload, separators=(",", ":"), sort_keys=True)


def idle_policy_from_annotation(raw: Optional[str]) -> Optional[IdlePolicy]:
    """Decode an annotation; ``None`` when absent, unreadable, or invalid.

    Fail-safe rules mirror the ingress parser: unknown fields, null, trailing
    data, pairing violations, and out-of-range timeouts all resolve to no
    policy. Deliberately stricter than ingress on ``activeProbe``: the wake
    gate treats it as opaque JSON, the pause side must eventually execute it,
    so an untypable probe invalidates the whole policy here (fail-safe both
    ways — never auto-paused, wake still follows the annotation).
    """
    if raw is None:
        return None
    text = raw.strip()
    if not text or text == "null":
        return None
    try:
        payload = json.loads(text)
    except (json.JSONDecodeError, TypeError):
        return None
    if not isinstance(payload, dict):
        return None

    known = {"idleTimeoutSeconds", "onIdle", "wakeOnAccess", "activeProbe"}
    if set(payload) - known:
        return None

    timeout_raw = payload.get("idleTimeoutSeconds")
    on_idle_raw = payload.get("onIdle")
    idle_timeout_seconds: Optional[int] = None
    if timeout_raw is not None:
        if not isinstance(timeout_raw, int) or isinstance(timeout_raw, bool):
            return None
        if not IDLE_TIMEOUT_MIN_SECONDS <= timeout_raw <= IDLE_TIMEOUT_MAX_SECONDS:
            return None
        if on_idle_raw != ON_IDLE_ACTION_PAUSE:
            return None
        idle_timeout_seconds = timeout_raw
    elif on_idle_raw is not None:
        return None

    wake_raw = payload.get("wakeOnAccess")
    if wake_raw is not None and not isinstance(wake_raw, bool):
        return None

    active_probe: Optional[ActivityProbe] = None
    probe_raw = payload.get("activeProbe")
    if probe_raw is not None:
        active_probe = _parse_probe(probe_raw)
        if active_probe is None:
            return None

    return IdlePolicy(
        idle_timeout_seconds=idle_timeout_seconds,
        on_idle=on_idle_raw if idle_timeout_seconds is not None else None,
        wake_on_access=True if wake_raw is None else wake_raw,
        active_probe=active_probe,
    )


def _parse_probe(raw: Any) -> Optional[ActivityProbe]:
    if not isinstance(raw, dict):
        return None
    if set(raw) - {"port", "path", "timeoutSeconds"}:
        return None
    port = raw.get("port")
    path = raw.get("path")
    timeout = raw.get("timeoutSeconds", DEFAULT_PROBE_TIMEOUT_SECONDS)
    if not isinstance(port, int) or isinstance(port, bool) or not 1 <= port <= 65535:
        return None
    if not isinstance(path, str) or not _probe_path_is_valid(path):
        return None
    if (
        not isinstance(timeout, int)
        or isinstance(timeout, bool)
        or not PROBE_TIMEOUT_MIN_SECONDS <= timeout <= PROBE_TIMEOUT_MAX_SECONDS
    ):
        return None
    return ActivityProbe(port=port, path=path, timeout_seconds=timeout)
