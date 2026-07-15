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
    from ..models.credential_auth_metadata import CredentialAuthMetadata
    from ..models.credential_match import CredentialMatch


T = TypeVar("T", bound="CredentialBindingMetadata")


@_attrs_define
class CredentialBindingMetadata:
    """
    Attributes:
        name (str):
        revision (int):
        match (CredentialMatch | Unset):
        auth (CredentialAuthMetadata | Unset):
    """

    name: str
    revision: int
    match: CredentialMatch | Unset = UNSET
    auth: CredentialAuthMetadata | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        name = self.name

        revision = self.revision

        match: dict[str, Any] | Unset = UNSET
        if not isinstance(self.match, Unset):
            match = self.match.to_dict()

        auth: dict[str, Any] | Unset = UNSET
        if not isinstance(self.auth, Unset):
            auth = self.auth.to_dict()

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "name": name,
                "revision": revision,
            }
        )
        if match is not UNSET:
            field_dict["match"] = match
        if auth is not UNSET:
            field_dict["auth"] = auth

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.credential_auth_metadata import CredentialAuthMetadata
        from ..models.credential_match import CredentialMatch

        d = dict(src_dict)
        name = d.pop("name")

        revision = d.pop("revision")

        _match = d.pop("match", UNSET)
        match: CredentialMatch | Unset
        if isinstance(_match, Unset):
            match = UNSET
        else:
            match = CredentialMatch.from_dict(_match)

        _auth = d.pop("auth", UNSET)
        auth: CredentialAuthMetadata | Unset
        if isinstance(_auth, Unset):
            auth = UNSET
        else:
            auth = CredentialAuthMetadata.from_dict(_auth)

        credential_binding_metadata = cls(
            name=name,
            revision=revision,
            match=match,
            auth=auth,
        )

        return credential_binding_metadata
