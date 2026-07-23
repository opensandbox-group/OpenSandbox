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

from ..models.credential_substitution_in_item import CredentialSubstitutionInItem

T = TypeVar("T", bound="CredentialSubstitution")


@_attrs_define
class CredentialSubstitution:
    """Literal case-sensitive placeholder replacement for selected request surfaces. Replacement is opt-in and scoped per
    binding.

        Attributes:
            credential (str): Name of the sandbox-local credential used as the replacement value.
            placeholder (str): Literal placeholder to replace. The placeholder and resolved credential value are added to
                the active redaction set.
            in_ (list[CredentialSubstitutionInItem]): Request surfaces where the placeholder may be replaced. Query and path
                replacements are URL-encoded, JSON string bodies are escaped, x-www-form-urlencoded bodies are form-encoded,
                compressed bodies are skipped, and multipart bodies are skipped.
    """

    credential: str
    placeholder: str
    in_: list[CredentialSubstitutionInItem]

    def to_dict(self) -> dict[str, Any]:
        credential = self.credential

        placeholder = self.placeholder

        in_ = []
        for in_item_data in self.in_:
            in_item = in_item_data.value
            in_.append(in_item)

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "credential": credential,
                "placeholder": placeholder,
                "in": in_,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        credential = d.pop("credential")

        placeholder = d.pop("placeholder")

        in_ = []
        _in_ = d.pop("in")
        for in_item_data in _in_:
            in_item = CredentialSubstitutionInItem(in_item_data)

            in_.append(in_item)

        credential_substitution = cls(
            credential=credential,
            placeholder=placeholder,
            in_=in_,
        )

        return credential_substitution
