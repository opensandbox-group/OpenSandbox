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
from typing import Any, TypeVar

from attrs import define as _attrs_define

T = TypeVar("T", bound="Host")


@_attrs_define
class Host:
    r"""Host path bind mount backend. Maps a directory on the host filesystem
    into the container. Only available when the runtime supports host mounts.

    Security note: Host paths are restricted by server-side allowlist.
    Users must specify paths under permitted prefixes.

        Attributes:
            path (str): Absolute path on the host filesystem to mount.
                Must start with '/' (Unix) or a drive letter such as 'C:\' or 'D:/'
                (Windows), and be under an allowed prefix.
    """

    path: str

    def to_dict(self) -> dict[str, Any]:
        path = self.path

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "path": path,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        path = d.pop("path")

        host = cls(
            path=path,
        )

        return host
