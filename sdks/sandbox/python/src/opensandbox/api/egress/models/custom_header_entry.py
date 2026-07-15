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

T = TypeVar("T", bound="CustomHeaderEntry")


@_attrs_define
class CustomHeaderEntry:
    """
    Attributes:
        name (str):
        credential (str):
    """

    name: str
    credential: str

    def to_dict(self) -> dict[str, Any]:
        name = self.name

        credential = self.credential

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "name": name,
                "credential": credential,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        name = d.pop("name")

        credential = d.pop("credential")

        custom_header_entry = cls(
            name=name,
            credential=credential,
        )

        return custom_header_entry
