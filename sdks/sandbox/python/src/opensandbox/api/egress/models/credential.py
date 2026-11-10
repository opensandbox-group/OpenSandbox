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
    from ..models.credential_source import CredentialSource


T = TypeVar("T", bound="Credential")


@_attrs_define
class Credential:
    """
    Attributes:
        name (str):
        source (CredentialSource): Where the credential material comes from. Exactly one kind, selected
            by `type`:

            - `inline`: the secret value travels in this request via `value`.
              Inline values are write-only and never appear in reads.
            - `exchange`: no local material. Egress resolves header sets per
              request from a header-resolver service addressed by `endpoint`.
              Supported endpoint forms are `unix:///absolute/path` (HTTP over a
              Unix domain socket) and `https://host[:port]` (TLS); anything else
              is rejected with 400.

            Fields of the other kind must be absent; for example `value` together
            with `type: exchange` is rejected with 400.
    """

    name: str
    source: CredentialSource

    def to_dict(self) -> dict[str, Any]:
        name = self.name

        source = self.source.to_dict()

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "name": name,
                "source": source,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.credential_source import CredentialSource

        d = dict(src_dict)
        name = d.pop("name")

        source = CredentialSource.from_dict(d.pop("source"))

        credential = cls(
            name=name,
            source=source,
        )

        return credential
