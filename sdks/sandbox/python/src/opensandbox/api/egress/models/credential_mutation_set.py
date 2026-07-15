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
from typing import TYPE_CHECKING, Any, TypeVar, cast

from attrs import define as _attrs_define

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.credential import Credential


T = TypeVar("T", bound="CredentialMutationSet")


@_attrs_define
class CredentialMutationSet:
    """
    Attributes:
        add (list[Credential] | Unset):
        replace (list[Credential] | Unset):
        delete (list[str] | Unset):
    """

    add: list[Credential] | Unset = UNSET
    replace: list[Credential] | Unset = UNSET
    delete: list[str] | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        add: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.add, Unset):
            add = []
            for add_item_data in self.add:
                add_item = add_item_data.to_dict()
                add.append(add_item)

        replace: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.replace, Unset):
            replace = []
            for replace_item_data in self.replace:
                replace_item = replace_item_data.to_dict()
                replace.append(replace_item)

        delete: list[str] | Unset = UNSET
        if not isinstance(self.delete, Unset):
            delete = self.delete

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if add is not UNSET:
            field_dict["add"] = add
        if replace is not UNSET:
            field_dict["replace"] = replace
        if delete is not UNSET:
            field_dict["delete"] = delete

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.credential import Credential

        d = dict(src_dict)
        _add = d.pop("add", UNSET)
        add: list[Credential] | Unset = UNSET
        if _add is not UNSET:
            add = []
            for add_item_data in _add:
                add_item = Credential.from_dict(add_item_data)

                add.append(add_item)

        _replace = d.pop("replace", UNSET)
        replace: list[Credential] | Unset = UNSET
        if _replace is not UNSET:
            replace = []
            for replace_item_data in _replace:
                replace_item = Credential.from_dict(replace_item_data)

                replace.append(replace_item)

        delete = cast(list[str], d.pop("delete", UNSET))

        credential_mutation_set = cls(
            add=add,
            replace=replace,
            delete=delete,
        )

        return credential_mutation_set
