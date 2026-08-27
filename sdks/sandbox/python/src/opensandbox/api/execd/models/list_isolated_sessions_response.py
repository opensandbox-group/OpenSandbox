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
from typing import TYPE_CHECKING, Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

if TYPE_CHECKING:
    from ..models.isolated_session_summary import IsolatedSessionSummary


T = TypeVar("T", bound="ListIsolatedSessionsResponse")


@_attrs_define
class ListIsolatedSessionsResponse:
    """
    Attributes:
        sessions (list[IsolatedSessionSummary]):
    """

    sessions: list[IsolatedSessionSummary]
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        sessions = []
        for sessions_item_data in self.sessions:
            sessions_item = sessions_item_data.to_dict()
            sessions.append(sessions_item)

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "sessions": sessions,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.isolated_session_summary import IsolatedSessionSummary

        d = dict(src_dict)
        sessions = []
        _sessions = d.pop("sessions")
        for sessions_item_data in _sessions:
            sessions_item = IsolatedSessionSummary.from_dict(sessions_item_data)

            sessions.append(sessions_item)

        list_isolated_sessions_response = cls(
            sessions=sessions,
        )

        list_isolated_sessions_response.additional_properties = d
        return list_isolated_sessions_response

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
