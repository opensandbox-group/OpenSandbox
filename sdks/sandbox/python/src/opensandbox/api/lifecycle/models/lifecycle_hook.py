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
from typing import Any, TypeVar, cast

from attrs import define as _attrs_define

from ..types import UNSET, Unset

T = TypeVar("T", bound="LifecycleHook")


@_attrs_define
class LifecycleHook:
    """A lifecycle command executed directly as an argv array. No implicit shell
    expansion is performed. Use an explicit shell command such as
    `["sh", "-c", "..."]` when shell syntax is required.

        Attributes:
            command (list[str]): Command and arguments to execute.
            timeout_seconds (int | Unset): Maximum execution time in seconds, up to 3 hours (10800 seconds) for `preStart`.
                The server defaults to 60 when omitted.
    """

    command: list[str]
    timeout_seconds: int | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        command = self.command

        timeout_seconds = self.timeout_seconds

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "command": command,
            }
        )
        if timeout_seconds is not UNSET:
            field_dict["timeoutSeconds"] = timeout_seconds

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        command = cast(list[str], d.pop("command"))

        timeout_seconds = d.pop("timeoutSeconds", UNSET)

        lifecycle_hook = cls(
            command=command,
            timeout_seconds=timeout_seconds,
        )

        return lifecycle_hook
