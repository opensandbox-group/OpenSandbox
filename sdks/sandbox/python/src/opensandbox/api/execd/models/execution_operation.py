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
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field
from dateutil.parser import isoparse

from ..models.execution_operation_kind import ExecutionOperationKind
from ..models.execution_operation_state import ExecutionOperationState

T = TypeVar("T", bound="ExecutionOperation")


@_attrs_define
class ExecutionOperation:
    """
    Attributes:
        id (str): Reserved command or PTY handle; may not yet be observable while creating.
        kind (ExecutionOperationKind):
        state (ExecutionOperationState): Creation only. created does not imply the command completed or succeeded;
            failed is retained and never reattempted with this identity.
        expires_at (datetime.datetime): End of the minimum recovery window, extended while creating or active.
    """

    id: str
    kind: ExecutionOperationKind
    state: ExecutionOperationState
    expires_at: datetime.datetime
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        id = self.id

        kind = self.kind.value

        state = self.state.value

        expires_at = self.expires_at.isoformat()

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "id": id,
                "kind": kind,
                "state": state,
                "expires_at": expires_at,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        id = d.pop("id")

        kind = ExecutionOperationKind(d.pop("kind"))

        state = ExecutionOperationState(d.pop("state"))

        expires_at = isoparse(d.pop("expires_at"))

        execution_operation = cls(
            id=id,
            kind=kind,
            state=state,
            expires_at=expires_at,
        )

        execution_operation.additional_properties = d
        return execution_operation

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
