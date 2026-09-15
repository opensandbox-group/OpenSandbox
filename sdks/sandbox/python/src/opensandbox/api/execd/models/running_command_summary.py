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
from typing import Any, Literal, TypeVar, cast

from attrs import define as _attrs_define
from dateutil.parser import isoparse

T = TypeVar("T", bound="RunningCommandSummary")


@_attrs_define
class RunningCommandSummary:
    """A command that is still running. Terminal fields are omitted.

    Attributes:
        session (str): Command identity; equal to the legacy command status `id`.
        running (Literal[True]):
        background (bool):
        started_at (datetime.datetime):
    """

    session: str
    running: Literal[True]
    background: bool
    started_at: datetime.datetime

    def to_dict(self) -> dict[str, Any]:
        session = self.session

        running = self.running

        background = self.background

        started_at = self.started_at.isoformat()

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "session": session,
                "running": running,
                "background": background,
                "started_at": started_at,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        session = d.pop("session")

        running = cast(Literal[True], d.pop("running"))
        if running != True:
            raise ValueError(f"running must match const True, got '{running}'")

        background = d.pop("background")

        started_at = isoparse(d.pop("started_at"))

        running_command_summary = cls(
            session=session,
            running=running,
            background=background,
            started_at=started_at,
        )

        return running_command_summary
