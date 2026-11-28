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
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="Permission")


@_attrs_define
class Permission:
    """File ownership and mode settings

    Attributes:
        mode (int): Octal permission digits 0-7777 written as a decimal integer (e.g., 644 or 4755). The 4000/2000/1000
            digits apply setuid/setgid/sticky bits. Explicit 0 requests mode 0000. Explicit directory modes apply only to
            new target directories; isolated-session operations keep session ownership. Invalid digits or values outside
            this range return 400 INVALID_FILE_METADATA before any target changes. Default: 755. Example: 755.
        owner (str | Unset): Owner username Example: root.
        group (str | Unset): Group name Example: root.
    """

    mode: int = 755
    owner: str | Unset = UNSET
    group: str | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        mode = self.mode

        owner = self.owner

        group = self.group

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "mode": mode,
            }
        )
        if owner is not UNSET:
            field_dict["owner"] = owner
        if group is not UNSET:
            field_dict["group"] = group

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        mode = d.pop("mode")

        owner = d.pop("owner", UNSET)

        group = d.pop("group", UNSET)

        permission = cls(
            mode=mode,
            owner=owner,
            group=group,
        )

        permission.additional_properties = d
        return permission

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
