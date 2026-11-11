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

import datetime
from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from dateutil.parser import isoparse

T = TypeVar("T", bound="RenewSandboxExpirationRequest")


@_attrs_define
class RenewSandboxExpirationRequest:
    """
    Attributes:
        expires_at (datetime.datetime): New absolute expiration time in UTC (RFC 3339 format).
            Must be in the future. May be earlier than, equal to, or later than the current expiresAt.
            The creation-time limit `server.max_sandbox_timeout_seconds` does not apply.

            Example: "2025-11-16T14:30:45Z"
    """

    expires_at: datetime.datetime

    def to_dict(self) -> dict[str, Any]:
        expires_at = self.expires_at.isoformat()

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "expiresAt": expires_at,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        expires_at = isoparse(d.pop("expiresAt"))

        renew_sandbox_expiration_request = cls(
            expires_at=expires_at,
        )

        return renew_sandbox_expiration_request
