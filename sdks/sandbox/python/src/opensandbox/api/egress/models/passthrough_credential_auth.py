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

from ..models.passthrough_credential_auth_type import PassthroughCredentialAuthType
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.credential_substitution import CredentialSubstitution


T = TypeVar("T", bound="PassthroughCredentialAuth")


@_attrs_define
class PassthroughCredentialAuth:
    """
    Attributes:
        type_ (PassthroughCredentialAuthType):
        substitutions (list[CredentialSubstitution] | Unset):
    """

    type_: PassthroughCredentialAuthType
    substitutions: list[CredentialSubstitution] | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        type_ = self.type_.value

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
            }
        )
        if substitutions is not UNSET:
            field_dict["substitutions"] = substitutions

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.credential_substitution import CredentialSubstitution

        d = dict(src_dict)
        type_ = PassthroughCredentialAuthType(d.pop("type"))

        _substitutions = d.pop("substitutions", UNSET)
        substitutions: list[CredentialSubstitution] | Unset = UNSET
        if _substitutions is not UNSET:
            substitutions = []
            for substitutions_item_data in _substitutions:
                substitutions_item = CredentialSubstitution.from_dict(substitutions_item_data)

                substitutions.append(substitutions_item)

        passthrough_credential_auth = cls(
            type_=type_,
            substitutions=substitutions,
        )

        return passthrough_credential_auth
