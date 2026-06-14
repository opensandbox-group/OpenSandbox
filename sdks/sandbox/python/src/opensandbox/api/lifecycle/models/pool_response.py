#
# Copyright 2025 Alibaba Group Holding Ltd.
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
from typing import TYPE_CHECKING, Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field
from dateutil.parser import isoparse

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.pool_capacity_spec import ApiPoolCapacitySpec
    from ..models.pool_status import ApiPoolStatus

T = TypeVar("T", bound="ApiPoolResponse")


@_attrs_define
class ApiPoolResponse:
    """Full representation of a Pool resource.

    Attributes:
        name: Unique pool name.
        capacity_spec: Capacity configuration.
        status: Observed runtime state (may be absent before first reconcile).
        created_at: Pool creation timestamp.
    """

    name: str
    capacity_spec: ApiPoolCapacitySpec
    status: ApiPoolStatus | Unset = UNSET
    created_at: datetime.datetime | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        from ..models.pool_capacity_spec import ApiPoolCapacitySpec

        capacity_spec = self.capacity_spec.to_dict()

        status: dict[str, Any] | Unset = UNSET
        if not isinstance(self.status, Unset):
            status = self.status.to_dict()

        created_at: str | Unset = UNSET
        if not isinstance(self.created_at, Unset):
            created_at = self.created_at.isoformat()

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "name": self.name,
                "capacitySpec": capacity_spec,
            }
        )
        if status is not UNSET:
            field_dict["status"] = status
        if created_at is not UNSET:
            field_dict["createdAt"] = created_at
        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.pool_capacity_spec import ApiPoolCapacitySpec
        from ..models.pool_status import ApiPoolStatus

        d = dict(src_dict)
        name = d.pop("name")
        capacity_spec = ApiPoolCapacitySpec.from_dict(d.pop("capacitySpec"))

        _status = d.pop("status", UNSET)
        status: ApiPoolStatus | Unset
        if isinstance(_status, Unset):
            status = UNSET
        else:
            status = ApiPoolStatus.from_dict(_status)

        _created_at = d.pop("createdAt", UNSET)
        created_at: datetime.datetime | Unset
        if isinstance(_created_at, Unset):
            created_at = UNSET
        else:
            created_at = isoparse(_created_at)

        obj = cls(
            name=name,
            capacity_spec=capacity_spec,
            status=status,
            created_at=created_at,
        )
        obj.additional_properties = d
        return obj

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
