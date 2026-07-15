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
    from ..models.inline_credential_source import InlineCredentialSource


T = TypeVar("T", bound="Credential")


@_attrs_define
class Credential:
    """
    Attributes:
        name (str):
        source (InlineCredentialSource):
    """

    name: str
    source: InlineCredentialSource

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
        from ..models.inline_credential_source import InlineCredentialSource

        d = dict(src_dict)
        name = d.pop("name")

        source = InlineCredentialSource.from_dict(d.pop("source"))

        credential = cls(
            name=name,
            source=source,
        )

        return credential
