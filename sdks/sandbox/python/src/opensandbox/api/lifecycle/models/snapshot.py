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

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar

from attrs import define as _attrs_define
from dateutil.parser import isoparse

from ..models.snapshot_mode import SnapshotMode
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.snapshot_status import SnapshotStatus


T = TypeVar("T", bound="Snapshot")


@_attrs_define
class Snapshot:
    """Persistent point-in-time capture of a sandbox.

    Attributes:
        id (str): Unique snapshot identifier
        sandbox_id (str): Source sandbox identifier used to create this snapshot
        status (SnapshotStatus): Detailed snapshot status information with lifecycle state and transition details.
        created_at (datetime.datetime): Snapshot creation timestamp
        name (str | Unset): Optional human-readable snapshot name
        mode (SnapshotMode | Unset): Artifact class of the snapshot: `full` is the runtime's most
            complete capture (memory and process state on the fast-sandbox
            backend; filesystem-only on Docker and Kubernetes),
            `rootfsOnly` preserves only the writable filesystem. Snapshots
            created before this field existed report `full`.
    """

    id: str
    sandbox_id: str
    status: SnapshotStatus
    created_at: datetime.datetime
    name: str | Unset = UNSET
    mode: SnapshotMode | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        id = self.id

        sandbox_id = self.sandbox_id

        status = self.status.to_dict()

        created_at = self.created_at.isoformat()

        name = self.name

        mode: str | Unset = UNSET
        if not isinstance(self.mode, Unset):
            mode = self.mode.value

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "id": id,
                "sandboxId": sandbox_id,
                "status": status,
                "createdAt": created_at,
            }
        )
        if name is not UNSET:
            field_dict["name"] = name
        if mode is not UNSET:
            field_dict["mode"] = mode

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.snapshot_status import SnapshotStatus

        d = dict(src_dict)
        id = d.pop("id")

        sandbox_id = d.pop("sandboxId")

        status = SnapshotStatus.from_dict(d.pop("status"))

        created_at = isoparse(d.pop("createdAt"))

        name = d.pop("name", UNSET)

        _mode = d.pop("mode", UNSET)
        mode: SnapshotMode | Unset
        if isinstance(_mode, Unset):
            mode = UNSET
        else:
            mode = SnapshotMode(_mode)

        snapshot = cls(
            id=id,
            sandbox_id=sandbox_id,
            status=status,
            created_at=created_at,
            name=name,
            mode=mode,
        )

        return snapshot
