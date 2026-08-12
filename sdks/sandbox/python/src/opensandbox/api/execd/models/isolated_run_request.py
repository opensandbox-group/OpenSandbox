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
from typing import TYPE_CHECKING, Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.isolated_run_request_envs import IsolatedRunRequestEnvs


T = TypeVar("T", bound="IsolatedRunRequest")


@_attrs_define
class IsolatedRunRequest:
    """
    Attributes:
        code (str): Shell code to execute inside the session Example: echo hello.
        envs (IsolatedRunRequestEnvs | Unset): Environment variables exported into the shell before the code runs
        timeout_seconds (int | Unset): Foreground-only timeout. The run is interrupted after this many
            seconds. Ignored for background runs: a background run is not
            time-limited, and idle GC is suspended while it is active, so a
            long-running background command keeps the session alive until it
            finishes (then the normal idle window applies) or the session is
            deleted.
             Example: 60.
        background (bool | Unset): When true (default false), start the code detached inside the
            session and return a 202 run handle instead of streaming output.
            The run's combined stdout/stderr is captured to a log file that
            can be polled via the run logs endpoint; its lifecycle is tracked
            via the run status endpoint.
            Background runs share the session's process group, so session-
            level signals (for example the SIGINT sent when a foreground run
            times out or is cancelled) also reach them; execd cannot signal
            individual in-namespace processes.
            Background runs require a writable log location, so sessions with
            a read-only (`ro`) workspace reject them with 400: there is no
            host-visible writable location for the run's log and exit-code
            files. rw and overlay workspaces are supported.
    """

    code: str
    envs: IsolatedRunRequestEnvs | Unset = UNSET
    timeout_seconds: int | Unset = UNSET
    background: bool | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        code = self.code

        envs: dict[str, Any] | Unset = UNSET
        if not isinstance(self.envs, Unset):
            envs = self.envs.to_dict()

        timeout_seconds = self.timeout_seconds

        background = self.background

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "code": code,
            }
        )
        if envs is not UNSET:
            field_dict["envs"] = envs
        if timeout_seconds is not UNSET:
            field_dict["timeout_seconds"] = timeout_seconds
        if background is not UNSET:
            field_dict["background"] = background

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.isolated_run_request_envs import IsolatedRunRequestEnvs

        d = dict(src_dict)
        code = d.pop("code")

        _envs = d.pop("envs", UNSET)
        envs: IsolatedRunRequestEnvs | Unset
        if isinstance(_envs, Unset):
            envs = UNSET
        else:
            envs = IsolatedRunRequestEnvs.from_dict(_envs)

        timeout_seconds = d.pop("timeout_seconds", UNSET)

        background = d.pop("background", UNSET)

        isolated_run_request = cls(
            code=code,
            envs=envs,
            timeout_seconds=timeout_seconds,
            background=background,
        )

        isolated_run_request.additional_properties = d
        return isolated_run_request

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
