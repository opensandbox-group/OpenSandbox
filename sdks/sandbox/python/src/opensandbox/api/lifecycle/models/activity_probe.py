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

from ..types import UNSET, Unset

T = TypeVar("T", bound="ActivityProbe")


@_attrs_define
class ActivityProbe:
    """Endpoint inside the sandbox that answers `{"active": bool,
    "message"?: string}` with 200 while work is in progress.

        Attributes:
            port (int): Sandbox port serving the probe endpoint.
            path (str): Absolute HTTP path of the probe endpoint. Must not contain
                whitespace or control characters.
            timeout_seconds (int | Unset): Probe request budget in seconds. The server defaults to 2 when omitted.
    """

    port: int
    path: str
    timeout_seconds: int | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        port = self.port

        path = self.path

        timeout_seconds = self.timeout_seconds

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "port": port,
                "path": path,
            }
        )
        if timeout_seconds is not UNSET:
            field_dict["timeoutSeconds"] = timeout_seconds

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        port = d.pop("port")

        path = d.pop("path")

        timeout_seconds = d.pop("timeoutSeconds", UNSET)

        activity_probe = cls(
            port=port,
            path=path,
            timeout_seconds=timeout_seconds,
        )

        return activity_probe
