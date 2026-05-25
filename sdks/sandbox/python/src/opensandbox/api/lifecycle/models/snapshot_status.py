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
from dateutil.parser import isoparse

from ..types import UNSET, Unset

T = TypeVar("T", bound="SnapshotStatus")


@_attrs_define
class SnapshotStatus:
    """Detailed snapshot status information with lifecycle state and transition details.

    Attributes:
        state (str): Snapshot lifecycle state.

            Common state values:
            - Creating: Snapshot creation has been accepted and runtime capture is in progress.
            - Deleting: Snapshot deletion has been requested and cleanup is in progress.
            - Ready: Snapshot is available for restoring sandboxes.
            - Failed: Snapshot creation failed.

            Note: New state values may be added in future versions.
            Clients should handle unknown state values gracefully.
        reason (str | Unset): Short machine-readable reason code for the current state.
            Examples: "snapshot_accepted", "snapshot_ready", "snapshot_capture_failed"
        message (str | Unset): Human-readable message describing the current state or failure reason
        last_transition_at (datetime.datetime | Unset): Timestamp of the last state transition
    """

    state: str
    reason: str | Unset = UNSET
    message: str | Unset = UNSET
    last_transition_at: datetime.datetime | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        state = self.state

        reason = self.reason

        message = self.message

        last_transition_at: str | Unset = UNSET
        if not isinstance(self.last_transition_at, Unset):
            last_transition_at = self.last_transition_at.isoformat()

        field_dict: dict[str, Any] = {}

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

        snapshot_status = cls(
            state=state,
            reason=reason,
            message=message,
            last_transition_at=last_transition_at,
        )

        return snapshot_status
