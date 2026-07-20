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

T = TypeVar("T", bound="BindMount")


@_attrs_define
class BindMount:
    """
    Attributes:
        source (str): Host path to bind-mount into the namespace.
        dest (str | Unset): Mount destination inside the namespace. Defaults to source when omitted.
        readonly (bool | Unset): When true the mount is read-only (--ro-bind); otherwise it is read-write (--bind).
            Default: False.
    """

    source: str
    dest: str | Unset = UNSET
    readonly: bool | Unset = False
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        source = self.source

        dest = self.dest

        readonly = self.readonly

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "source": source,
            }
        )
        if dest is not UNSET:
            field_dict["dest"] = dest
        if readonly is not UNSET:
            field_dict["readonly"] = readonly

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        source = d.pop("source")

        dest = d.pop("dest", UNSET)

        readonly = d.pop("readonly", UNSET)

        bind_mount = cls(
            source=source,
            dest=dest,
            readonly=readonly,
        )

        bind_mount.additional_properties = d
        return bind_mount

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
