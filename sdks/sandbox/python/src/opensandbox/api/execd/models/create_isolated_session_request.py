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

from ..models.create_isolated_session_request_profile import CreateIsolatedSessionRequestProfile
from ..models.create_isolated_session_request_uid_mode import CreateIsolatedSessionRequestUidMode
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.bind_mount import BindMount
    from ..models.env_passthrough_spec import EnvPassthroughSpec
    from ..models.isolated_workspace_spec import IsolatedWorkspaceSpec


T = TypeVar("T", bound="CreateIsolatedSessionRequest")


@_attrs_define
class CreateIsolatedSessionRequest:
    """
    Attributes:
        workspace (IsolatedWorkspaceSpec):
        profile (CreateIsolatedSessionRequestProfile | Unset):
        extra_writable (list[str] | Unset):
        binds (list[BindMount] | Unset): Additional host paths bind-mounted into the namespace with an explicit source-
            to-destination mapping. Unlike extra_writable (which mounts source==destination read-write), each entry may map
            a distinct destination path and be mounted read-only. The source path of every entry must fall within the
            configured writable allowlist.
        share_net (bool | Unset):
        env_passthrough (EnvPassthroughSpec | Unset):
        uid (int | Unset):
        gid (int | Unset):
        uid_mode (CreateIsolatedSessionRequestUidMode | Unset): Controls how user identity is established inside the
            namespace. "setpriv" (default) uses real setuid via setpriv(1). "userns" creates a user namespace via --unshare-
            user --disable-userns.
        idle_timeout_seconds (int | Unset):
    """

    workspace: IsolatedWorkspaceSpec
    profile: CreateIsolatedSessionRequestProfile | Unset = UNSET
    extra_writable: list[str] | Unset = UNSET
    binds: list[BindMount] | Unset = UNSET
    share_net: bool | Unset = UNSET
    env_passthrough: EnvPassthroughSpec | Unset = UNSET
    uid: int | Unset = UNSET
    gid: int | Unset = UNSET
    uid_mode: CreateIsolatedSessionRequestUidMode | Unset = UNSET
    idle_timeout_seconds: int | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        workspace = self.workspace.to_dict()

        profile: str | Unset = UNSET
        if not isinstance(self.profile, Unset):
            profile = self.profile.value

        extra_writable: list[str] | Unset = UNSET
        if not isinstance(self.extra_writable, Unset):
            extra_writable = self.extra_writable

        binds: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.binds, Unset):
            binds = []
            for binds_item_data in self.binds:
                binds_item = binds_item_data.to_dict()
                binds.append(binds_item)

        share_net = self.share_net

        env_passthrough: dict[str, Any] | Unset = UNSET
        if not isinstance(self.env_passthrough, Unset):
            env_passthrough = self.env_passthrough.to_dict()

        uid = self.uid

        gid = self.gid

        uid_mode: str | Unset = UNSET
        if not isinstance(self.uid_mode, Unset):
            uid_mode = self.uid_mode.value

        idle_timeout_seconds = self.idle_timeout_seconds

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "workspace": workspace,
            }
        )
        if profile is not UNSET:
            field_dict["profile"] = profile
        if extra_writable is not UNSET:
            field_dict["extra_writable"] = extra_writable
        if binds is not UNSET:
            field_dict["binds"] = binds
        if share_net is not UNSET:
            field_dict["share_net"] = share_net
        if env_passthrough is not UNSET:
            field_dict["env_passthrough"] = env_passthrough
        if uid is not UNSET:
            field_dict["uid"] = uid
        if gid is not UNSET:
            field_dict["gid"] = gid
        if uid_mode is not UNSET:
            field_dict["uid_mode"] = uid_mode
        if idle_timeout_seconds is not UNSET:
            field_dict["idle_timeout_seconds"] = idle_timeout_seconds

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.bind_mount import BindMount
        from ..models.env_passthrough_spec import EnvPassthroughSpec
        from ..models.isolated_workspace_spec import IsolatedWorkspaceSpec

        d = dict(src_dict)
        workspace = IsolatedWorkspaceSpec.from_dict(d.pop("workspace"))

        _profile = d.pop("profile", UNSET)
        profile: CreateIsolatedSessionRequestProfile | Unset
        if isinstance(_profile, Unset):
            profile = UNSET
        else:
            profile = CreateIsolatedSessionRequestProfile(_profile)

        extra_writable = cast(list[str], d.pop("extra_writable", UNSET))

        _binds = d.pop("binds", UNSET)
        binds: list[BindMount] | Unset = UNSET
        if _binds is not UNSET:
            binds = []
            for binds_item_data in _binds:
                binds_item = BindMount.from_dict(binds_item_data)

                binds.append(binds_item)

        share_net = d.pop("share_net", UNSET)

        _env_passthrough = d.pop("env_passthrough", UNSET)
        env_passthrough: EnvPassthroughSpec | Unset
        if isinstance(_env_passthrough, Unset):
            env_passthrough = UNSET
        else:
            env_passthrough = EnvPassthroughSpec.from_dict(_env_passthrough)

        uid = d.pop("uid", UNSET)

        gid = d.pop("gid", UNSET)

        _uid_mode = d.pop("uid_mode", UNSET)
        uid_mode: CreateIsolatedSessionRequestUidMode | Unset
        if isinstance(_uid_mode, Unset):
            uid_mode = UNSET
        else:
            uid_mode = CreateIsolatedSessionRequestUidMode(_uid_mode)

        idle_timeout_seconds = d.pop("idle_timeout_seconds", UNSET)

        create_isolated_session_request = cls(
            workspace=workspace,
            profile=profile,
            extra_writable=extra_writable,
            binds=binds,
            share_net=share_net,
            env_passthrough=env_passthrough,
            uid=uid,
            gid=gid,
            uid_mode=uid_mode,
            idle_timeout_seconds=idle_timeout_seconds,
        )

        create_isolated_session_request.additional_properties = d
        return create_isolated_session_request

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
