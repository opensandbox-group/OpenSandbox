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

if TYPE_CHECKING:
    from ..models.credential_binding_metadata import CredentialBindingMetadata
    from ..models.credential_metadata import CredentialMetadata


T = TypeVar("T", bound="CredentialVaultState")


@_attrs_define
class CredentialVaultState:
    """
    Attributes:
        revision (int):
        credentials (list[CredentialMetadata]):
        bindings (list[CredentialBindingMetadata]):
    """

    revision: int
    credentials: list[CredentialMetadata]
    bindings: list[CredentialBindingMetadata]

    def to_dict(self) -> dict[str, Any]:
        revision = self.revision

        credentials = []
        for credentials_item_data in self.credentials:
            credentials_item = credentials_item_data.to_dict()
            credentials.append(credentials_item)

        bindings = []
        for bindings_item_data in self.bindings:
            bindings_item = bindings_item_data.to_dict()
            bindings.append(bindings_item)

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "revision": revision,
                "credentials": credentials,
                "bindings": bindings,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.credential_binding_metadata import CredentialBindingMetadata
        from ..models.credential_metadata import CredentialMetadata

        d = dict(src_dict)
        revision = d.pop("revision")

        credentials = []
        _credentials = d.pop("credentials")
        for credentials_item_data in _credentials:
            credentials_item = CredentialMetadata.from_dict(credentials_item_data)

            credentials.append(credentials_item)

        bindings = []
        _bindings = d.pop("bindings")
        for bindings_item_data in _bindings:
            bindings_item = CredentialBindingMetadata.from_dict(bindings_item_data)

            bindings.append(bindings_item)

        credential_vault_state = cls(
            revision=revision,
            credentials=credentials,
            bindings=bindings,
        )

        return credential_vault_state
