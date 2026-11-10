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

from ..models.credential_source_type import CredentialSourceType
from ..types import UNSET, Unset

T = TypeVar("T", bound="CredentialSource")


@_attrs_define
class CredentialSource:
    """Where the credential material comes from. Exactly one kind, selected
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

        Attributes:
            type_ (CredentialSourceType):
            value (str | Unset): Inline secret value. Required when `type` is `inline`; must be
                absent otherwise.
            endpoint (str | Unset): Header-resolver endpoint. Required when `type` is `exchange`; must
                be absent otherwise.
    """

    type_: CredentialSourceType
    value: str | Unset = UNSET
    endpoint: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        type_ = self.type_.value

        value = self.value

        endpoint = self.endpoint

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "type": type_,
            }
        )
        if value is not UNSET:
            field_dict["value"] = value
        if endpoint is not UNSET:
            field_dict["endpoint"] = endpoint

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        type_ = CredentialSourceType(d.pop("type"))

        value = d.pop("value", UNSET)

        endpoint = d.pop("endpoint", UNSET)

        credential_source = cls(
            type_=type_,
            value=value,
            endpoint=endpoint,
        )

        return credential_source


# Redacted repr: the credential value is write-only and must never
# surface in logs, exception messages, or debug output. attrs would
# overwrite a class-level __repr__, so the override is applied on the
# class object after definition.
def _redacted_repr(self: CredentialSource) -> str:
    value = "***" if self.value is not UNSET else "UNSET"
    return (
        f"{type(self).__name__}(type_={self.type_!r}, value={value},"
        f" endpoint={self.endpoint!r})"
    )


CredentialSource.__repr__ = _redacted_repr  # type: ignore[method-assign]
