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
from typing import Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field
from dateutil.parser import isoparse

from ..models.session_state_status import SessionStateStatus
from ..types import UNSET, Unset

T = TypeVar("T", bound="SessionState")


@_attrs_define
class SessionState:
    """
    Attributes:
        status (SessionStateStatus | Unset):
        created_at (datetime.datetime | Unset):
        last_run_at (datetime.datetime | Unset):
        idle_remaining_seconds (int | None | Unset):
    """

    status: SessionStateStatus | Unset = UNSET
    created_at: datetime.datetime | Unset = UNSET
    last_run_at: datetime.datetime | Unset = UNSET
    idle_remaining_seconds: int | None | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        status: str | Unset = UNSET
        if not isinstance(self.status, Unset):
            status = self.status.value

        created_at: str | Unset = UNSET
        if not isinstance(self.created_at, Unset):
            created_at = self.created_at.isoformat()

        last_run_at: str | Unset = UNSET
        if not isinstance(self.last_run_at, Unset):
            last_run_at = self.last_run_at.isoformat()

        idle_remaining_seconds: int | None | Unset
        if isinstance(self.idle_remaining_seconds, Unset):
            idle_remaining_seconds = UNSET
        else:
            idle_remaining_seconds = self.idle_remaining_seconds

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update({})
        if status is not UNSET:
            field_dict["status"] = status
        if created_at is not UNSET:
            field_dict["created_at"] = created_at
        if last_run_at is not UNSET:
            field_dict["last_run_at"] = last_run_at
        if idle_remaining_seconds is not UNSET:
            field_dict["idle_remaining_seconds"] = idle_remaining_seconds

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        _status = d.pop("status", UNSET)
        status: SessionStateStatus | Unset
        if isinstance(_status, Unset):
            status = UNSET
        else:
            status = SessionStateStatus(_status)

        _created_at = d.pop("created_at", UNSET)
        created_at: datetime.datetime | Unset
        if isinstance(_created_at, Unset):
            created_at = UNSET
        else:
            created_at = isoparse(_created_at)

        _last_run_at = d.pop("last_run_at", UNSET)
        last_run_at: datetime.datetime | Unset
        if isinstance(_last_run_at, Unset):
            last_run_at = UNSET
        else:
            last_run_at = isoparse(_last_run_at)

        def _parse_idle_remaining_seconds(data: object) -> int | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(int | None | Unset, data)

        idle_remaining_seconds = _parse_idle_remaining_seconds(d.pop("idle_remaining_seconds", UNSET))

        session_state = cls(
            status=status,
            created_at=created_at,
            last_run_at=last_run_at,
            idle_remaining_seconds=idle_remaining_seconds,
        )

        session_state.additional_properties = d
        return session_state

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
