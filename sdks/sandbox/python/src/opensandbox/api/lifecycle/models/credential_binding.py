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

if TYPE_CHECKING:
    from ..models.credential_auth import CredentialAuth
    from ..models.credential_match import CredentialMatch


T = TypeVar("T", bound="CredentialBinding")


@_attrs_define
class CredentialBinding:
    """
    Attributes:
        name (str):
        match (CredentialMatch):
        auth (CredentialAuth): How a binding injects credential material. Exactly one auth kind:

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
    """

    name: str
    match: CredentialMatch
    auth: CredentialAuth

    def to_dict(self) -> dict[str, Any]:
        name = self.name

        match = self.match.to_dict()

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
        from ..models.credential_auth import CredentialAuth
        from ..models.credential_match import CredentialMatch

        d = dict(src_dict)
        name = d.pop("name")

        match = CredentialMatch.from_dict(d.pop("match"))

        auth = CredentialAuth.from_dict(d.pop("auth"))

        credential_binding = cls(
            name=name,
            match=match,
            auth=auth,
        )

        return credential_binding
