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

T = TypeVar("T", bound="PeriodicLifecycleHook")


@_attrs_define
class PeriodicLifecycleHook:
    """A named lifecycle command scheduled inside the sandbox by execd.

    Attributes:
        name (str): Name unique among periodic hooks in this sandbox.
        schedule (str): Five-field cron expression or descriptor such as `@hourly` or
            `@every 30s`. An `@every` interval must be a whole number of
            seconds with a minimum of one second.
        command (list[str]): Command and arguments to execute without implicit shell expansion.
        timeout_seconds (int | Unset): Maximum execution time in seconds, up to 300. The server defaults to 60 when
            omitted.
    """

    name: str
    schedule: str
    command: list[str]
    timeout_seconds: int | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        name = self.name

        schedule = self.schedule

        command = self.command

        timeout_seconds = self.timeout_seconds

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "name": name,
                "schedule": schedule,
                "command": command,
            }
        )
        if timeout_seconds is not UNSET:
            field_dict["timeoutSeconds"] = timeout_seconds

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        name = d.pop("name")

        schedule = d.pop("schedule")

        command = cast(list[str], d.pop("command"))

        timeout_seconds = d.pop("timeoutSeconds", UNSET)

        periodic_lifecycle_hook = cls(
            name=name,
            schedule=schedule,
            command=command,
            timeout_seconds=timeout_seconds,
        )

        return periodic_lifecycle_hook
