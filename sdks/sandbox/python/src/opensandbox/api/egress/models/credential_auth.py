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
from typing import TYPE_CHECKING, Any, TypeVar

from attrs import define as _attrs_define

from ..models.credential_auth_type import CredentialAuthType
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.credential_substitution import CredentialSubstitution
    from ..models.custom_header_entry import CustomHeaderEntry


T = TypeVar("T", bound="CredentialAuth")


@_attrs_define
class CredentialAuth:
    """How a binding injects credential material. Exactly one auth kind:

    - `bearer`: `type: bearer` plus `credential`.
    - `basic`: `type: basic` plus `credential`.
    - `apiKey`: `type: apiKey` plus `name` and `credential`.
    - `customHeaders`: `type: customHeaders` plus `headers`.
    - `passthrough`: `type: passthrough`, no credential reference.
    - typeless exchange: `type` omitted with only `credential` set. The
      referenced credential must be exchange-sourced; egress resolves the
      header set per request from the credential's endpoint.

    Static kinds must reference inline-sourced credentials, and the
    typeless form must reference an exchange-sourced credential and must
    not set `name`, `headers`, or `substitutions`. The five static kinds
    accept optional `substitutions`. Violations are rejected with 400.

        Attributes:
            type_ (CredentialAuthType | Unset): Static auth kind. Omit entirely for the typeless exchange form.
            credential (str | Unset): Name of the sandbox-local credential whose value is injected. For
                `basic` the referenced credential contains pre-encoded
                base64(username:password). Required for `bearer`, `basic`,
                `apiKey`, and the typeless exchange form; must be absent otherwise.
            name (str | Unset): Request header name. Required for `apiKey`; must be absent otherwise.
            headers (list[CustomHeaderEntry] | Unset): Static header set. Required for `customHeaders`; must be absent
                otherwise.
            substitutions (list[CredentialSubstitution] | Unset): Optional placeholder replacement set. Accepted for the
                five static
                kinds; must be absent for the typeless exchange form.
    """

    type_: CredentialAuthType | Unset = UNSET
    credential: str | Unset = UNSET
    name: str | Unset = UNSET
    headers: list[CustomHeaderEntry] | Unset = UNSET
    substitutions: list[CredentialSubstitution] | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        type_: str | Unset = UNSET
        if not isinstance(self.type_, Unset):
            type_ = self.type_.value

        credential = self.credential

        name = self.name

        headers: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.headers, Unset):
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

        field_dict.update({})
        if type_ is not UNSET:
            field_dict["type"] = type_
        if credential is not UNSET:
            field_dict["credential"] = credential
        if name is not UNSET:
            field_dict["name"] = name
        if headers is not UNSET:
            field_dict["headers"] = headers
        if substitutions is not UNSET:
            field_dict["substitutions"] = substitutions

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.credential_substitution import CredentialSubstitution
        from ..models.custom_header_entry import CustomHeaderEntry

        d = dict(src_dict)
        _type_ = d.pop("type", UNSET)
        type_: CredentialAuthType | Unset
        if isinstance(_type_, Unset):
            type_ = UNSET
        else:
            type_ = CredentialAuthType(_type_)

        credential = d.pop("credential", UNSET)

        name = d.pop("name", UNSET)

        _headers = d.pop("headers", UNSET)
        headers: list[CustomHeaderEntry] | Unset = UNSET
        if _headers is not UNSET:
            headers = []
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

        credential_auth = cls(
            type_=type_,
            credential=credential,
            name=name,
            headers=headers,
            substitutions=substitutions,
        )

        return credential_auth
