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

from ..types import UNSET, Unset

T = TypeVar("T", bound="CredentialRequestHeaderSelector")


@_attrs_define
class CredentialRequestHeaderSelector:
    """
    Attributes:
        name (str): RFC 9110 field-name token; matching is ASCII case-insensitive.
        value (str | Unset): Required on writes. Must not be empty after trimming outer SP and HTAB. The remaining value
            is matched case-sensitively and is never returned in binding metadata.
    """

    name: str
    value: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        name = self.name

        value = self.value

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "name": name,
            }
        )
        if value is not UNSET:
            field_dict["value"] = value

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        name = d.pop("name")

        value = d.pop("value", UNSET)

        credential_request_header_selector = cls(
            name=name,
            value=value,
        )

        return credential_request_header_selector
