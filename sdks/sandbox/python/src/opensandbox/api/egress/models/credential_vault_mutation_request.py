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

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.credential_binding_mutation_set import CredentialBindingMutationSet
    from ..models.credential_mutation_set import CredentialMutationSet


T = TypeVar("T", bound="CredentialVaultMutationRequest")


@_attrs_define
class CredentialVaultMutationRequest:
    """
    Attributes:
        expected_revision (int | Unset): Optional optimistic concurrency guard.
        credentials (CredentialMutationSet | Unset):
        bindings (CredentialBindingMutationSet | Unset):
    """

    expected_revision: int | Unset = UNSET
    credentials: CredentialMutationSet | Unset = UNSET
    bindings: CredentialBindingMutationSet | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        expected_revision = self.expected_revision

        credentials: dict[str, Any] | Unset = UNSET
        if not isinstance(self.credentials, Unset):
            credentials = self.credentials.to_dict()

        bindings: dict[str, Any] | Unset = UNSET
        if not isinstance(self.bindings, Unset):
            bindings = self.bindings.to_dict()

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if expected_revision is not UNSET:
            field_dict["expectedRevision"] = expected_revision
        if credentials is not UNSET:
            field_dict["credentials"] = credentials
        if bindings is not UNSET:
            field_dict["bindings"] = bindings

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.credential_binding_mutation_set import CredentialBindingMutationSet
        from ..models.credential_mutation_set import CredentialMutationSet

        d = dict(src_dict)
        expected_revision = d.pop("expectedRevision", UNSET)

        _credentials = d.pop("credentials", UNSET)
        credentials: CredentialMutationSet | Unset
        if isinstance(_credentials, Unset):
            credentials = UNSET
        else:
            credentials = CredentialMutationSet.from_dict(_credentials)

        _bindings = d.pop("bindings", UNSET)
        bindings: CredentialBindingMutationSet | Unset
        if isinstance(_bindings, Unset):
            bindings = UNSET
        else:
            bindings = CredentialBindingMutationSet.from_dict(_bindings)

        credential_vault_mutation_request = cls(
            expected_revision=expected_revision,
            credentials=credentials,
            bindings=bindings,
        )

        return credential_vault_mutation_request
