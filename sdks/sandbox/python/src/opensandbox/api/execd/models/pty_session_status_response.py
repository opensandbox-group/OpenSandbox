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

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="PTYSessionStatusResponse")


@_attrs_define
class PTYSessionStatusResponse:
    """
    Attributes:
        session_id (str):
        running (bool):
        output_offset (int):
        launch_attempted (bool | Unset): Whether a process launch was attempted. False means a dormant session. Omitted
            by older servers; absence does not mean false.
        launch_failed (bool | Unset): Whether the latest accepted launch attempt failed before starting a process. A
            nonzero process exit is not a launch failure. Caller-bound sessions cannot reattempt launch; their operation
            remains created because the session exists. Omitted by older servers.
    """

    session_id: str
    running: bool
    output_offset: int
    launch_attempted: bool | Unset = UNSET
    launch_failed: bool | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        session_id = self.session_id

        running = self.running

        output_offset = self.output_offset

        launch_attempted = self.launch_attempted

        launch_failed = self.launch_failed

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "session_id": session_id,
                "running": running,
                "output_offset": output_offset,
            }
        )
        if launch_attempted is not UNSET:
            field_dict["launch_attempted"] = launch_attempted
        if launch_failed is not UNSET:
            field_dict["launch_failed"] = launch_failed

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        session_id = d.pop("session_id")

        running = d.pop("running")

        output_offset = d.pop("output_offset")

        launch_attempted = d.pop("launch_attempted", UNSET)

        launch_failed = d.pop("launch_failed", UNSET)

        pty_session_status_response = cls(
            session_id=session_id,
            running=running,
            output_offset=output_offset,
            launch_attempted=launch_attempted,
            launch_failed=launch_failed,
        )

        pty_session_status_response.additional_properties = d
        return pty_session_status_response

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
