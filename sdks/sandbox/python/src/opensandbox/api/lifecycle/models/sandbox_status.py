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

from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field
from dateutil.parser import isoparse

from ..types import UNSET, Unset

T = TypeVar("T", bound="SandboxStatus")


@_attrs_define
class SandboxStatus:
    """Detailed status information with lifecycle state and transition details

    Attributes:
        state (str): High-level lifecycle state of the sandbox.

            Common state values:
            - Pending: Sandbox is being provisioned
            - Running: Sandbox is running and ready to accept requests
            - Pausing: Sandbox is in the process of pausing
            - Paused: Sandbox has been paused while retaining its state
            - Resuming: Sandbox is being restored after a pause
            - Stopping: Sandbox is being terminated
            - Terminated: Sandbox has been successfully terminated
            - Failed: Sandbox encountered a critical error

            State transitions:
            - Pending → Running (after creation completes)
            - Running → Pausing (when pause is requested)
            - Pausing → Paused (pause operation completes)
            - Paused → Resuming (when resume is requested)
            - Resuming → Running (when resume operation completes)
            - Running/Paused → Stopping (when kill is requested or TTL expires)
            - Stopping → Terminated (kill/timeout operation completes)
            - Pending/Running/Paused/Resuming → Failed (on error)

            Note: New state values may be added in future versions.
            Clients should handle unknown state values gracefully.
        reason (str | Unset): Short machine-readable reason code for the current state.
            Examples: "user_delete", "ttl_expiry", "provision_timeout", "runtime_error"
        message (str | Unset): Human-readable message describing the current state or reason for state transition
        last_transition_at (datetime.datetime | Unset): Timestamp of the last state transition
    """

    state: str
    reason: str | Unset = UNSET
    message: str | Unset = UNSET
    last_transition_at: datetime.datetime | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        state = self.state

        reason = self.reason

        message = self.message

        last_transition_at: str | Unset = UNSET
        if not isinstance(self.last_transition_at, Unset):
            last_transition_at = self.last_transition_at.isoformat()

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "state": state,
            }
        )
        if reason is not UNSET:
            field_dict["reason"] = reason
        if message is not UNSET:
            field_dict["message"] = message
        if last_transition_at is not UNSET:
            field_dict["lastTransitionAt"] = last_transition_at

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        state = d.pop("state")

        reason = d.pop("reason", UNSET)

        message = d.pop("message", UNSET)

        _last_transition_at = d.pop("lastTransitionAt", UNSET)
        last_transition_at: datetime.datetime | Unset
        if isinstance(_last_transition_at, Unset):
            last_transition_at = UNSET
        else:
            last_transition_at = isoparse(_last_transition_at)

        sandbox_status = cls(
            state=state,
            reason=reason,
            message=message,
            last_transition_at=last_transition_at,
        )

        sandbox_status.additional_properties = d
        return sandbox_status

    @property
    def additional_keys(self) -> list[str]:
        return list(self.additional_properties.keys())

    def __getitem__(self, key: str) -> Any:
        return self.additional_properties[key]

    def __setitem__(self, key: str, value: Any) -> None:
        self.additional_properties[key] = value

    def __delitem__(self, key: str) -> None:
        del self.additional_properties[key]

    def __contains__(self, key: str) -> bool:
        return key in self.additional_properties
