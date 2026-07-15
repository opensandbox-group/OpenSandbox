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
from attrs import field as _attrs_field
from dateutil.parser import isoparse

from ..models.file_info_type import FileInfoType
from ..types import UNSET, Unset

T = TypeVar("T", bound="FileInfo")


@_attrs_define
class FileInfo:
    """File metadata including path and permissions

    Attributes:
        path (str): Absolute file path Example: /workspace/file.txt.
        size (int): File size in bytes Example: 2048.
        modified_at (datetime.datetime): Last modification time Example: 2025-11-16 14:30:45+00:00.
        created_at (datetime.datetime): File creation time Example: 2025-11-16 14:30:45+00:00.
        owner (str): File owner username Example: admin.
        group (str): File group name Example: admin.
        mode (int): File permissions in octal format Example: 755.
        type_ (FileInfoType | Unset): Entry type Example: file.
    """

    path: str
    size: int
    modified_at: datetime.datetime
    created_at: datetime.datetime
    owner: str
    group: str
    mode: int
    type_: FileInfoType | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        path = self.path

        size = self.size

        modified_at = self.modified_at.isoformat()

        created_at = self.created_at.isoformat()

        owner = self.owner

        group = self.group

        mode = self.mode

        type_: str | Unset = UNSET
        if not isinstance(self.type_, Unset):
            type_ = self.type_.value

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "path": path,
                "size": size,
                "modified_at": modified_at,
                "created_at": created_at,
                "owner": owner,
                "group": group,
                "mode": mode,
            }
        )
        if type_ is not UNSET:
            field_dict["type"] = type_

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        path = d.pop("path")

        size = d.pop("size")

        modified_at = isoparse(d.pop("modified_at"))

        created_at = isoparse(d.pop("created_at"))

        owner = d.pop("owner")

        group = d.pop("group")

        mode = d.pop("mode")

        _type_ = d.pop("type", UNSET)
        type_: FileInfoType | Unset
        if isinstance(_type_, Unset):
            type_ = UNSET
        else:
            type_ = FileInfoType(_type_)

        file_info = cls(
            path=path,
            size=size,
            modified_at=modified_at,
            created_at=created_at,
            owner=owner,
            group=group,
            mode=mode,
            type_=type_,
        )

        file_info.additional_properties = d
        return file_info

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
