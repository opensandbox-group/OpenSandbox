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

T = TypeVar("T", bound="CapabilitiesResponse")


@_attrs_define
class CapabilitiesResponse:
    """
    Attributes:
        available (bool | Unset):
        isolator (str | Unset):
        version (str | Unset):
        message (str | Unset): Diagnostic message when isolation is unavailable
        commit_supported (bool | Unset):
        diff_supported (bool | Unset):
    """

    available: bool | Unset = UNSET
    isolator: str | Unset = UNSET
    version: str | Unset = UNSET
    message: str | Unset = UNSET
    commit_supported: bool | Unset = UNSET
    diff_supported: bool | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        available = self.available

        isolator = self.isolator

        version = self.version

        message = self.message

        commit_supported = self.commit_supported

        diff_supported = self.diff_supported

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update({})
        if available is not UNSET:
            field_dict["available"] = available
        if isolator is not UNSET:
            field_dict["isolator"] = isolator
        if version is not UNSET:
            field_dict["version"] = version
        if message is not UNSET:
            field_dict["message"] = message
        if commit_supported is not UNSET:
            field_dict["commit_supported"] = commit_supported
        if diff_supported is not UNSET:
            field_dict["diff_supported"] = diff_supported

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        available = d.pop("available", UNSET)

        isolator = d.pop("isolator", UNSET)

        version = d.pop("version", UNSET)

        message = d.pop("message", UNSET)

        commit_supported = d.pop("commit_supported", UNSET)

        diff_supported = d.pop("diff_supported", UNSET)

        capabilities_response = cls(
            available=available,
            isolator=isolator,
            version=version,
            message=message,
            commit_supported=commit_supported,
            diff_supported=diff_supported,
        )

        capabilities_response.additional_properties = d
        return capabilities_response

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
