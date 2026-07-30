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

from ..models.custom_headers_credential_auth_type import CustomHeadersCredentialAuthType
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.credential_substitution import CredentialSubstitution
    from ..models.custom_header_entry import CustomHeaderEntry


T = TypeVar("T", bound="CustomHeadersCredentialAuth")


@_attrs_define
class CustomHeadersCredentialAuth:
    """
    Attributes:
        type_ (CustomHeadersCredentialAuthType):
        headers (list[CustomHeaderEntry]):
        substitutions (list[CredentialSubstitution] | Unset):
    """

    type_: CustomHeadersCredentialAuthType
    headers: list[CustomHeaderEntry]
    substitutions: list[CredentialSubstitution] | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        type_ = self.type_.value

        headers = []
        for headers_item_data in self.headers:
            headers_item = headers_item_data.to_dict()
            headers.append(headers_item)

        substitutions: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.substitutions, Unset):
            substitutions = []
            for substitutions_item_data in self.substitutions:
                substitutions_item = substitutions_item_data.to_dict()
                substitutions.append(substitutions_item)

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "type": type_,
                "headers": headers,
            }
        )
        if substitutions is not UNSET:
            field_dict["substitutions"] = substitutions

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.credential_substitution import CredentialSubstitution
        from ..models.custom_header_entry import CustomHeaderEntry

        d = dict(src_dict)
        type_ = CustomHeadersCredentialAuthType(d.pop("type"))

        headers = []
        _headers = d.pop("headers")
        for headers_item_data in _headers:
            headers_item = CustomHeaderEntry.from_dict(headers_item_data)

            headers.append(headers_item)

        _substitutions = d.pop("substitutions", UNSET)
        substitutions: list[CredentialSubstitution] | Unset = UNSET
        if _substitutions is not UNSET:
            substitutions = []
            for substitutions_item_data in _substitutions:
                substitutions_item = CredentialSubstitution.from_dict(substitutions_item_data)

                substitutions.append(substitutions_item)

        custom_headers_credential_auth = cls(
            type_=type_,
            headers=headers,
            substitutions=substitutions,
        )

        return custom_headers_credential_auth
