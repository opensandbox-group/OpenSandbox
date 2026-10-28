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
from typing import Any, TypeVar

from attrs import define as _attrs_define

from ..models.inline_credential_source_type import InlineCredentialSourceType

T = TypeVar("T", bound="InlineCredentialSource")


@_attrs_define
class InlineCredentialSource:
    """
    Attributes:
        type_ (InlineCredentialSourceType):
        value (str):
    """

    type_: InlineCredentialSourceType
    value: str

    def to_dict(self) -> dict[str, Any]:
        type_ = self.type_.value

        value = self.value

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "type": type_,
                "value": value,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        type_ = InlineCredentialSourceType(d.pop("type"))

        value = d.pop("value")

        inline_credential_source = cls(
            type_=type_,
            value=value,
        )

        return inline_credential_source


# Redacted repr: the credential value is write-only and must never
# surface in logs, exception messages, or debug output. attrs would
# overwrite a class-level __repr__, so the override is applied on the
# class object after definition.
def _redacted_repr(self: InlineCredentialSource) -> str:
    return f"{type(self).__name__}(value=***)"


InlineCredentialSource.__repr__ = _redacted_repr  # type: ignore[method-assign]
