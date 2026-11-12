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

T = TypeVar("T", bound="RenameFileItem")


@_attrs_define
class RenameFileItem:
    """File rename/move operation. Both src and dest must be non-empty. On ordinary filesystem routes, including execution-
    identity routes, paths must also remain non-empty after environment-variable and home-directory expansion; isolated-
    session paths are literal. Missing or empty paths, or empty expansions on ordinary routes, return 400
    INVALID_REQUEST_BODY before any rename in the batch. Other path-expansion and filesystem errors retain their
    existing handling and can leave earlier renames applied.

        Attributes:
            src (str): Source file path Example: /workspace/old.txt.
            dest (str): Destination file path Example: /workspace/new.txt.
    """

    src: str
    dest: str
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        src = self.src

        dest = self.dest

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "src": src,
                "dest": dest,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        src = d.pop("src")

        dest = d.pop("dest")

        rename_file_item = cls(
            src=src,
            dest=dest,
        )

        rename_file_item.additional_properties = d
        return rename_file_item

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
