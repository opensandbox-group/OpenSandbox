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

from ..models.create_snapshot_request_mode import CreateSnapshotRequestMode
from ..types import UNSET, Unset

T = TypeVar("T", bound="CreateSnapshotRequest")


@_attrs_define
class CreateSnapshotRequest:
    """Optional settings for creating a sandbox snapshot.

    Attributes:
        name (str | Unset): Optional human-readable snapshot name.
        mode (CreateSnapshotRequestMode | Unset): What the snapshot persists.

            - `full` (default): captures the writable filesystem plus memory and
              process state; restoring resumes the sandbox in place.
            - `rootfsOnly`: captures only the writable filesystem; restoring
              boots a fresh sandbox from the disk. Running processes and
              in-memory state are discarded, and the entrypoint re-runs on
              restore. Supported only on the fast-sandbox backend with
              OCI-backed sandboxes; rejected otherwise.
             Default: CreateSnapshotRequestMode.FULL.
    """

    name: str | Unset = UNSET
    mode: CreateSnapshotRequestMode | Unset = CreateSnapshotRequestMode.FULL

    def to_dict(self) -> dict[str, Any]:
        name = self.name

        mode: str | Unset = UNSET
        if not isinstance(self.mode, Unset):
            mode = self.mode.value

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if name is not UNSET:
            field_dict["name"] = name
        if mode is not UNSET:
            field_dict["mode"] = mode

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        name = d.pop("name", UNSET)

        _mode = d.pop("mode", UNSET)
        mode: CreateSnapshotRequestMode | Unset
        if isinstance(_mode, Unset):
            mode = UNSET
        else:
            mode = CreateSnapshotRequestMode(_mode)

        create_snapshot_request = cls(
            name=name,
            mode=mode,
        )

        return create_snapshot_request
