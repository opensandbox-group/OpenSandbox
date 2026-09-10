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
from typing import TYPE_CHECKING, Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.run_command_request_envs import RunCommandRequestEnvs


T = TypeVar("T", bound="RunCommandRequest")


@_attrs_define
class RunCommandRequest:
    """Execute exactly one of command (shell text) or argv (native arguments).

    Attributes:
        command (str | Unset): Shell command to execute. Mutually exclusive with argv. Example: ls -la /workspace.
        argv (list[str] | Unset): Executable and literal arguments, mutually exclusive with command. argv[0] must be
            non-empty; NUL is invalid and arguments are not shell-expanded. Relative paths use cwd; bare names search
            absolute entries in the child PATH. Windows uses standard argument encoding and adds .exe to extensionless
            names; batch files require a shell.
             Example: ['python3', '-c', "print('hello')"].
        cwd (str | Unset): Working directory, defaulting to the daemon directory. Expands $NAME and ${NAME} using the
            command environment, and leading ~ using the daemon user's home. Undefined variables fail validation.
             Example: /workspace.
        background (bool | Unset): Whether to run command in detached mode Default: False.
        timeout (int | Unset): Maximum allowed execution time in milliseconds before the command is forcefully
            terminated by the server. If omitted, the server will not enforce any timeout. Example: 60000.
        uid (int | Unset): Unix user ID used to run the command. If `gid` is provided, `uid` is required.
             Example: 1000.
        gid (int | Unset): Unix group ID used to run the command. Requires `uid` to be provided.
             Example: 1000.
        envs (RunCommandRequestEnvs | Unset): Literal request values overriding EXECD_ENVS and daemon variables, in that
            order. Names are case-insensitive on Windows.
             Example: {'PATH': '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin', 'PYTHONUNBUFFERED': '1'}.
    """

    command: str | Unset = UNSET
    argv: list[str] | Unset = UNSET
    cwd: str | Unset = UNSET
    background: bool | Unset = False
    timeout: int | Unset = UNSET
    uid: int | Unset = UNSET
    gid: int | Unset = UNSET
    envs: RunCommandRequestEnvs | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        command = self.command

        argv: list[str] | Unset = UNSET
        if not isinstance(self.argv, Unset):
            argv = self.argv

        cwd = self.cwd

        background = self.background

        timeout = self.timeout

        uid = self.uid

        gid = self.gid

        envs: dict[str, Any] | Unset = UNSET
        if not isinstance(self.envs, Unset):
            envs = self.envs.to_dict()

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update({})
        if command is not UNSET:
            field_dict["command"] = command
        if argv is not UNSET:
            field_dict["argv"] = argv
        if cwd is not UNSET:
            field_dict["cwd"] = cwd
        if background is not UNSET:
            field_dict["background"] = background
        if timeout is not UNSET:
            field_dict["timeout"] = timeout
        if uid is not UNSET:
            field_dict["uid"] = uid
        if gid is not UNSET:
            field_dict["gid"] = gid
        if envs is not UNSET:
            field_dict["envs"] = envs

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.run_command_request_envs import RunCommandRequestEnvs

        d = dict(src_dict)
        command = d.pop("command", UNSET)

        argv = cast(list[str], d.pop("argv", UNSET))

        cwd = d.pop("cwd", UNSET)

        background = d.pop("background", UNSET)

        timeout = d.pop("timeout", UNSET)

        uid = d.pop("uid", UNSET)

        gid = d.pop("gid", UNSET)

        _envs = d.pop("envs", UNSET)
        envs: RunCommandRequestEnvs | Unset
        if isinstance(_envs, Unset):
            envs = UNSET
        else:
            envs = RunCommandRequestEnvs.from_dict(_envs)

        run_command_request = cls(
            command=command,
            argv=argv,
            cwd=cwd,
            background=background,
            timeout=timeout,
            uid=uid,
            gid=gid,
            envs=envs,
        )

        run_command_request.additional_properties = d
        return run_command_request

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
