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
from uuid import UUID

from attrs import define as _attrs_define
from attrs import field as _attrs_field
from dateutil.parser import isoparse

from ..types import UNSET, Unset

T = TypeVar("T", bound="IsolatedRunStatus")


@_attrs_define
class IsolatedRunStatus:
    """Lifecycle state of an isolated background run

    Attributes:
        session_id (UUID):
        run_id (UUID):
        running (bool): Whether the run is still executing
        started_at (datetime.datetime): Run start time in RFC3339 format Example: 2025-12-22T09:08:05Z.
        exit_code (int | None | Unset): Exit code of the code if the run has finished
        error (str | Unset): Error message if the run failed (e.g. session terminated) Example: session terminated.
        finished_at (datetime.datetime | None | Unset): Run finish time in RFC3339 format (null if still running)
            Example: 2025-12-22T09:08:09Z.
    """

    session_id: UUID
    run_id: UUID
    running: bool
    started_at: datetime.datetime
    exit_code: int | None | Unset = UNSET
    error: str | Unset = UNSET
    finished_at: datetime.datetime | None | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        session_id = str(self.session_id)

        run_id = str(self.run_id)

        running = self.running

        started_at = self.started_at.isoformat()

        exit_code: int | None | Unset
        if isinstance(self.exit_code, Unset):
            exit_code = UNSET
        else:
            exit_code = self.exit_code

        error = self.error

        finished_at: None | str | Unset
        if isinstance(self.finished_at, Unset):
            finished_at = UNSET
        elif isinstance(self.finished_at, datetime.datetime):
            finished_at = self.finished_at.isoformat()
        else:
            finished_at = self.finished_at

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "session_id": session_id,
                "run_id": run_id,
                "running": running,
                "started_at": started_at,
            }
        )
        if exit_code is not UNSET:
            field_dict["exit_code"] = exit_code
        if error is not UNSET:
            field_dict["error"] = error
        if finished_at is not UNSET:
            field_dict["finished_at"] = finished_at

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        session_id = UUID(d.pop("session_id"))

        run_id = UUID(d.pop("run_id"))

        running = d.pop("running")

        started_at = isoparse(d.pop("started_at"))

        def _parse_exit_code(data: object) -> int | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(int | None | Unset, data)

        exit_code = _parse_exit_code(d.pop("exit_code", UNSET))

        error = d.pop("error", UNSET)

        def _parse_finished_at(data: object) -> datetime.datetime | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            try:
                if not isinstance(data, str):
                    raise TypeError()
                finished_at_type_0 = isoparse(data)

                return finished_at_type_0
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            return cast(datetime.datetime | None | Unset, data)

        finished_at = _parse_finished_at(d.pop("finished_at", UNSET))

        isolated_run_status = cls(
            session_id=session_id,
            run_id=run_id,
            running=running,
            started_at=started_at,
            exit_code=exit_code,
            error=error,
            finished_at=finished_at,
        )

        isolated_run_status.additional_properties = d
        return isolated_run_status

    @property
    def additional_keys(self) -> list[str]:
        return list(self.additional_properties.keys())

    def __getitem__(self, key: str) -> Any:
        return self.additional_properties[key]

    def __setitem__(self, key: str, value: Any) -> None:
        self.additional_properties[key] = value

    def __delitem__(self, key: str) -> None:
        del self.additional_properties[key]

    def __contains__(self, key: str) -> bool:
        return key in self.additional_properties
