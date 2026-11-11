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

T = TypeVar("T", bound="CredentialAuthMetadata")


@_attrs_define
class CredentialAuthMetadata:
    """Sanitized auth shape — `type` (plus `name` for `apiKey`) for static kinds; `credential` only for the typeless
    exchange form.

        Attributes:
            type_ (str | Unset):
            name (str | Unset):
            credential (str | Unset):
    """

    type_: str | Unset = UNSET
    name: str | Unset = UNSET
    credential: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        type_ = self.type_

        name = self.name

        credential = self.credential

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if type_ is not UNSET:
            field_dict["type"] = type_
        if name is not UNSET:
            field_dict["name"] = name
        if credential is not UNSET:
            field_dict["credential"] = credential

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        type_ = d.pop("type", UNSET)

        name = d.pop("name", UNSET)

        credential = d.pop("credential", UNSET)

        credential_auth_metadata = cls(
            type_=type_,
            name=name,
            credential=credential,
        )

        return credential_auth_metadata
