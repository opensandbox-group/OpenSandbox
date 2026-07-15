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
    from ..models.api_key_credential_auth import ApiKeyCredentialAuth
    from ..models.basic_credential_auth import BasicCredentialAuth
    from ..models.bearer_credential_auth import BearerCredentialAuth
    from ..models.credential_match import CredentialMatch
    from ..models.custom_headers_credential_auth import CustomHeadersCredentialAuth
    from ..models.passthrough_credential_auth import PassthroughCredentialAuth


T = TypeVar("T", bound="CredentialBinding")


@_attrs_define
class CredentialBinding:
    """
    Attributes:
        name (str):
        match (CredentialMatch):
        auth (ApiKeyCredentialAuth | BasicCredentialAuth | BearerCredentialAuth | CustomHeadersCredentialAuth |
            PassthroughCredentialAuth):
    """

    name: str
    match: CredentialMatch
    auth: (
        ApiKeyCredentialAuth
        | BasicCredentialAuth
        | BearerCredentialAuth
        | CustomHeadersCredentialAuth
        | PassthroughCredentialAuth
    )

    def to_dict(self) -> dict[str, Any]:
        from ..models.api_key_credential_auth import ApiKeyCredentialAuth
        from ..models.basic_credential_auth import BasicCredentialAuth
        from ..models.bearer_credential_auth import BearerCredentialAuth
        from ..models.custom_headers_credential_auth import CustomHeadersCredentialAuth

        name = self.name

        match = self.match.to_dict()

        auth: dict[str, Any]
        if isinstance(self.auth, BearerCredentialAuth):
            auth = self.auth.to_dict()
        elif isinstance(self.auth, BasicCredentialAuth):
            auth = self.auth.to_dict()
        elif isinstance(self.auth, ApiKeyCredentialAuth):
            auth = self.auth.to_dict()
        elif isinstance(self.auth, CustomHeadersCredentialAuth):
            auth = self.auth.to_dict()
        else:
            auth = self.auth.to_dict()

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "name": name,
                "match": match,
                "auth": auth,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.api_key_credential_auth import ApiKeyCredentialAuth
        from ..models.basic_credential_auth import BasicCredentialAuth
        from ..models.bearer_credential_auth import BearerCredentialAuth
        from ..models.credential_match import CredentialMatch
        from ..models.custom_headers_credential_auth import CustomHeadersCredentialAuth
        from ..models.passthrough_credential_auth import PassthroughCredentialAuth

        d = dict(src_dict)
        name = d.pop("name")

        match = CredentialMatch.from_dict(d.pop("match"))

        def _parse_auth(
            data: object,
        ) -> (
            ApiKeyCredentialAuth
            | BasicCredentialAuth
            | BearerCredentialAuth
            | CustomHeadersCredentialAuth
            | PassthroughCredentialAuth
        ):
            try:
                if not isinstance(data, dict):
                    raise TypeError()
                componentsschemas_credential_auth_type_0 = BearerCredentialAuth.from_dict(data)

                return componentsschemas_credential_auth_type_0
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            try:
                if not isinstance(data, dict):
                    raise TypeError()
                componentsschemas_credential_auth_type_1 = BasicCredentialAuth.from_dict(data)

                return componentsschemas_credential_auth_type_1
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            try:
                if not isinstance(data, dict):
                    raise TypeError()
                componentsschemas_credential_auth_type_2 = ApiKeyCredentialAuth.from_dict(data)

                return componentsschemas_credential_auth_type_2
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            try:
                if not isinstance(data, dict):
                    raise TypeError()
                componentsschemas_credential_auth_type_3 = CustomHeadersCredentialAuth.from_dict(data)

                return componentsschemas_credential_auth_type_3
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            if not isinstance(data, dict):
                raise TypeError()
            componentsschemas_credential_auth_type_4 = PassthroughCredentialAuth.from_dict(data)

            return componentsschemas_credential_auth_type_4

        auth = _parse_auth(d.pop("auth"))

        credential_binding = cls(
            name=name,
            match=match,
            auth=auth,
        )

        return credential_binding
