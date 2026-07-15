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
    from ..models.replace_file_content_result import ReplaceFileContentResult


T = TypeVar("T", bound="ReplaceContentResponse200")


@_attrs_define
class ReplaceContentResponse200:
    """ """

    additional_properties: dict[str, ReplaceFileContentResult] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        field_dict: dict[str, Any] = {}
        for prop_name, prop in self.additional_properties.items():
            field_dict[prop_name] = prop.to_dict()

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.replace_file_content_result import ReplaceFileContentResult

        d = dict(src_dict)
        replace_content_response_200 = cls()

        additional_properties = {}
        for prop_name, prop_dict in d.items():
            additional_property = ReplaceFileContentResult.from_dict(prop_dict)

            additional_properties[prop_name] = additional_property

        replace_content_response_200.additional_properties = additional_properties
        return replace_content_response_200

    @property
    def additional_keys(self) -> list[str]:
        return list(self.additional_properties.keys())

    def __getitem__(self, key: str) -> ReplaceFileContentResult:
        return self.additional_properties[key]

    def __setitem__(self, key: str, value: ReplaceFileContentResult) -> None:
        self.additional_properties[key] = value

    def __delitem__(self, key: str) -> None:
        del self.additional_properties[key]

    def __contains__(self, key: str) -> bool:
        return key in self.additional_properties
