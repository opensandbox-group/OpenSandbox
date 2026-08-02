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

from ..types import UNSET, Unset

T = TypeVar("T", bound="TerminalCommandSummary")


@_attrs_define
class TerminalCommandSummary:
    """A command that has finished. `exit_code` is present and may be null.

    Attributes:
        session (str): Command identity; equal to the legacy command status `id`.
        running (Literal[False]):
        background (bool):
        started_at (datetime.datetime):
        finished_at (datetime.datetime):
        exit_code (int | None):
        error (str | Unset):
    """

    session: str
    running: Literal[False]
    background: bool
    started_at: datetime.datetime
    finished_at: datetime.datetime
    exit_code: int | None
    error: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        session = self.session

        running = self.running

        background = self.background

        started_at = self.started_at.isoformat()

        finished_at = self.finished_at.isoformat()

        exit_code: int | None
        exit_code = self.exit_code

        error = self.error

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "session": session,
                "running": running,
                "background": background,
                "started_at": started_at,
                "finished_at": finished_at,
                "exit_code": exit_code,
            }
        )
        if error is not UNSET:
            field_dict["error"] = error

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        session = d.pop("session")

        running = cast(Literal[False], d.pop("running"))
        if running != False:
            raise ValueError(f"running must match const False, got '{running}'")

        background = d.pop("background")

        started_at = isoparse(d.pop("started_at"))

        finished_at = isoparse(d.pop("finished_at"))

        def _parse_exit_code(data: object) -> int | None:
            if data is None:
                return data
            return cast(int | None, data)

        exit_code = _parse_exit_code(d.pop("exit_code"))

        error = d.pop("error", UNSET)

        terminal_command_summary = cls(
            session=session,
            running=running,
            background=background,
            started_at=started_at,
            finished_at=finished_at,
            exit_code=exit_code,
            error=error,
        )

        return terminal_command_summary
