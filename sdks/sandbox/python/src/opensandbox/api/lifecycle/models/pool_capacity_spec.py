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

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="ApiPoolCapacitySpec")


@_attrs_define
class ApiPoolCapacitySpec:
    """API model for pool capacity configuration.

    Attributes:
        buffer_max: Maximum warm-buffer size.
        buffer_min: Minimum warm-buffer size.
        pool_max: Maximum total pool size.
        pool_min: Minimum total pool size.
    """

    buffer_max: int
    buffer_min: int
    pool_max: int
    pool_min: int
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "bufferMax": self.buffer_max,
                "bufferMin": self.buffer_min,
                "poolMax": self.pool_max,
                "poolMin": self.pool_min,
            }
        )
        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        buffer_max = d.pop("bufferMax")
        buffer_min = d.pop("bufferMin")
        pool_max = d.pop("poolMax")
        pool_min = d.pop("poolMin")
        obj = cls(
            buffer_max=buffer_max,
            buffer_min=buffer_min,
            pool_max=pool_max,
            pool_min=pool_min,
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
