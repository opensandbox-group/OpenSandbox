#
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
#

from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar

from attrs import define as _attrs_define

from ..models.idle_policy_on_idle import IdlePolicyOnIdle
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.activity_probe import ActivityProbe


T = TypeVar("T", bound="IdlePolicy")


@_attrs_define
class IdlePolicy:
    """Idle behavior declared by the sandbox (OSEP-0024). The policy is
    desired state: the server validates it and persists it as the
    reserved `sandbox.opensandbox.io/idle-policy` Sandbox CR annotation.

    With `idleTimeoutSeconds`, the sandbox is auto-paused after that many
    seconds without ingress-observed activity. Without it, the policy is
    wake-only: the sandbox is never auto-paused, but a (manually) paused
    sandbox wakes on access. A missing, malformed, or invalid policy is
    treated exactly as absent — never as an implicit opt-in.

    Constraints: `idleTimeoutSeconds` must not exceed the server's
    configured activity TTL (default 1800), so a present-but-stale
    activity key always implies idle-by-value. Enabling deployment flags
    never changes the behavior of sandboxes that do not declare a policy.

        Attributes:
            idle_timeout_seconds (int | Unset): Idle threshold X in seconds: the sandbox is auto-paused after X
                continuously idle seconds. Absent together with `onIdle` means a
                wake-only policy.
            on_idle (IdlePolicyOnIdle | Unset): Action taken at the idle threshold. Required with, and only
                valid with, `idleTimeoutSeconds`. Only `pause` exists today; the
                enum exists so future actions are additive.
            wake_on_access (bool | Unset): Whether incoming traffic may wake the sandbox from `Paused`.
                Defaults to `true` when omitted.
                `false` keeps today's paused behavior (permanent 503 through the
                gateway) and only `POST /sandboxes/{sandboxId}/resume` restores
                the sandbox.
            active_probe (ActivityProbe | Unset): Endpoint inside the sandbox that answers `{"active": bool,
                "message"?: string}` with 200 while work is in progress.
    """

    idle_timeout_seconds: int | Unset = UNSET
    on_idle: IdlePolicyOnIdle | Unset = UNSET
    wake_on_access: bool | Unset = UNSET
    active_probe: ActivityProbe | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        idle_timeout_seconds = self.idle_timeout_seconds

        on_idle: str | Unset = UNSET
        if not isinstance(self.on_idle, Unset):
            on_idle = self.on_idle.value

        wake_on_access = self.wake_on_access

        active_probe: dict[str, Any] | Unset = UNSET
        if not isinstance(self.active_probe, Unset):
            active_probe = self.active_probe.to_dict()

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if idle_timeout_seconds is not UNSET:
            field_dict["idleTimeoutSeconds"] = idle_timeout_seconds
        if on_idle is not UNSET:
            field_dict["onIdle"] = on_idle
        if wake_on_access is not UNSET:
            field_dict["wakeOnAccess"] = wake_on_access
        if active_probe is not UNSET:
            field_dict["activeProbe"] = active_probe

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.activity_probe import ActivityProbe

        d = dict(src_dict)
        idle_timeout_seconds = d.pop("idleTimeoutSeconds", UNSET)

        _on_idle = d.pop("onIdle", UNSET)
        on_idle: IdlePolicyOnIdle | Unset
        if isinstance(_on_idle, Unset):
            on_idle = UNSET
        else:
            on_idle = IdlePolicyOnIdle(_on_idle)

        wake_on_access = d.pop("wakeOnAccess", UNSET)

        _active_probe = d.pop("activeProbe", UNSET)
        active_probe: ActivityProbe | Unset
        if isinstance(_active_probe, Unset):
            active_probe = UNSET
        else:
            active_probe = ActivityProbe.from_dict(_active_probe)

        idle_policy = cls(
            idle_timeout_seconds=idle_timeout_seconds,
            on_idle=on_idle,
            wake_on_access=wake_on_access,
            active_probe=active_probe,
        )

        return idle_policy
