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

import datetime
from collections.abc import Mapping
from typing import Any, TypeVar, cast

from attrs import define as _attrs_define
from dateutil.parser import isoparse

from ..models.diagnostic_content_response_delivery import DiagnosticContentResponseDelivery
from ..models.diagnostic_content_response_kind import DiagnosticContentResponseKind
from ..types import UNSET, Unset

T = TypeVar("T", bound="DiagnosticContentResponse")


@_attrs_define
class DiagnosticContentResponse:
    """Descriptor for diagnostic text content.

    When `delivery` is `inline`, servers MUST include `content` with the
    diagnostic text and MUST omit `contentUrl` / `expiresAt`.

    When `delivery` is `url`, servers MUST include `contentUrl` and `expiresAt`
    to identify where the diagnostic text can be downloaded and MUST omit
    `content`.

        Attributes:
            sandbox_id (str): Unique sandbox identifier.
            kind (DiagnosticContentResponseKind): Diagnostic payload kind.
            scope (str): Diagnostic scope used for this response.
            delivery (DiagnosticContentResponseDelivery): How the diagnostic text payload is delivered.
            content_type (str): Media type of the diagnostic payload.
            truncated (bool): Whether the diagnostic payload returned inline or by URL was
                intentionally truncated by the server. This does not indicate backend
                retention gaps such as expired Kubernetes Events; those should be
                reported through `warnings` when available.
            content (str | Unset): Inline diagnostic text payload. Present when `delivery` is `inline`.
            content_url (str | Unset): URL where the diagnostic text payload can be downloaded. Present when `delivery` is
                `url`.
            content_length (int | Unset): Payload size in bytes when known.
            expires_at (datetime.datetime | Unset): Expiration time for the download URL. Present when `delivery` is `url`.
            warnings (list[str] | Unset): Non-fatal warnings about payload completeness or availability.
    """

    sandbox_id: str
    kind: DiagnosticContentResponseKind
    scope: str
    delivery: DiagnosticContentResponseDelivery
    content_type: str
    truncated: bool
    content: str | Unset = UNSET
    content_url: str | Unset = UNSET
    content_length: int | Unset = UNSET
    expires_at: datetime.datetime | Unset = UNSET
    warnings: list[str] | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        sandbox_id = self.sandbox_id

        kind = self.kind.value

        scope = self.scope

        delivery = self.delivery.value

        content_type = self.content_type

        truncated = self.truncated

        content = self.content

        content_url = self.content_url

        content_length = self.content_length

        expires_at: str | Unset = UNSET
        if not isinstance(self.expires_at, Unset):
            expires_at = self.expires_at.isoformat()

        warnings: list[str] | Unset = UNSET
        if not isinstance(self.warnings, Unset):
            warnings = self.warnings

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "sandboxId": sandbox_id,
                "kind": kind,
                "scope": scope,
                "delivery": delivery,
                "contentType": content_type,
                "truncated": truncated,
            }
        )
        if content is not UNSET:
            field_dict["content"] = content
        if content_url is not UNSET:
            field_dict["contentUrl"] = content_url
        if content_length is not UNSET:
            field_dict["contentLength"] = content_length
        if expires_at is not UNSET:
            field_dict["expiresAt"] = expires_at
        if warnings is not UNSET:
            field_dict["warnings"] = warnings

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        sandbox_id = d.pop("sandboxId")

        kind = DiagnosticContentResponseKind(d.pop("kind"))

        scope = d.pop("scope")

        delivery = DiagnosticContentResponseDelivery(d.pop("delivery"))

        content_type = d.pop("contentType")

        truncated = d.pop("truncated")

        content = d.pop("content", UNSET)

        content_url = d.pop("contentUrl", UNSET)

        content_length = d.pop("contentLength", UNSET)

        _expires_at = d.pop("expiresAt", UNSET)
        expires_at: datetime.datetime | Unset
        if isinstance(_expires_at, Unset):
            expires_at = UNSET
        else:
            expires_at = isoparse(_expires_at)

        warnings = cast(list[str], d.pop("warnings", UNSET))

        diagnostic_content_response = cls(
            sandbox_id=sandbox_id,
            kind=kind,
            scope=scope,
            delivery=delivery,
            content_type=content_type,
            truncated=truncated,
            content=content,
            content_url=content_url,
            content_length=content_length,
            expires_at=expires_at,
            warnings=warnings,
        )

        return diagnostic_content_response
