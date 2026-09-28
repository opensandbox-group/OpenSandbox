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
from attrs import field as _attrs_field

from ..models.isolated_overlay_spec_mode import IsolatedOverlaySpecMode
from ..types import UNSET, Unset

T = TypeVar("T", bound="IsolatedOverlaySpec")


@_attrs_define
class IsolatedOverlaySpec:
    """One overlay mount inside the isolated namespace. `mode=overlay` mounts a copy-on-write view: with `persist=true`
    (default) writes land in a host upper directory tracked and usage-accounted by execd (the substrate for the
    diff/commit endpoints); with `persist=false` the upper is an ephemeral tmpfs whose writes are discarded when the
    session ends. `rw` and `ro` bind the host path directly; `persist` applies to overlay mode only and must be omitted
    for them (execd rejects the request otherwise).

        Attributes:
            path (str): Mount destination inside the namespace (absolute). Example: /workspace.
            mode (IsolatedOverlaySpecMode | Unset): Mount mode. Defaults to `overlay`.
            persist (bool | Unset): Overlay mode only. When true (default; execd treats an omitted value as true) the copy-
                on-write upper is a host directory allocated per session; when false it is an ephemeral tmpfs whose writes are
                discarded when the session ends. Must be omitted for `rw` and `ro` modes (execd rejects the request otherwise).
                Because an ephemeral upper lives inside the namespace only, the filesystem API serves `persist=false` overlays
                from their host-side (lower) content: in-session writes under such an overlay are not observable through the
                files API and files-API writes into the overlay are rejected. Overlay mounts with `persist=false` also cannot
                host background-run logs, so background runs are rejected unless the first overlay is `rw` or `overlay` with
                `persist=true`.
    """

    path: str
    mode: IsolatedOverlaySpecMode | Unset = UNSET
    persist: bool | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        path = self.path

        mode: str | Unset = UNSET
        if not isinstance(self.mode, Unset):
            mode = self.mode.value

        persist = self.persist

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "path": path,
            }
        )
        if mode is not UNSET:
            field_dict["mode"] = mode
        if persist is not UNSET:
            field_dict["persist"] = persist

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        path = d.pop("path")

        _mode = d.pop("mode", UNSET)
        mode: IsolatedOverlaySpecMode | Unset
        if isinstance(_mode, Unset):
            mode = UNSET
        else:
            mode = IsolatedOverlaySpecMode(_mode)

        persist = d.pop("persist", UNSET)

        isolated_overlay_spec = cls(
            path=path,
            mode=mode,
            persist=persist,
        )

        isolated_overlay_spec.additional_properties = d
        return isolated_overlay_spec

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
