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
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="CreatePTYOperationRequest")


@_attrs_define
class CreatePTYOperationRequest:
    """
    Attributes:
        operation_id (str): Optional caller identity: <instance_id>.<issued_at Unix seconds>.<8-128 character random
            token>. Obtain instance_id and issued_at from GET /execution/instance, generate token and persist the entire ID
            before sending. Scope is the authenticated execd controller and kind; clients sharing its configured token share
            one principal. Recovery lasts 24 hours from issued_at, extended while creating or active. Default capacity is
            4096 records/controller, configurable at startup with --operation-capacity or EXECD_OPERATION_CAPACITY and
            advertised by instance discovery; new claims fail closed at capacity. Expired terminal/dormant records are
            removed; old expired IDs return 410 instead of recreating. Controller restart or another sandbox returns 409
            operation_instance_mismatch: outcome unknown. Memory and OS process creation are not a transaction. No cross-
            execd-restart reconciliation, exactly-once completion, or business-side-effect guarantee. Never regenerate any
            identity component during retry. Use an opaque random token, never a secret or business payload.
        cwd (str | Unset):
        command (str | Unset):
    """

    operation_id: str
    cwd: str | Unset = UNSET
    command: str | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        operation_id = self.operation_id

        cwd = self.cwd

        command = self.command

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "operation_id": operation_id,
            }
        )
        if cwd is not UNSET:
            field_dict["cwd"] = cwd
        if command is not UNSET:
            field_dict["command"] = command

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        operation_id = d.pop("operation_id")

        cwd = d.pop("cwd", UNSET)

        command = d.pop("command", UNSET)

        create_pty_operation_request = cls(
            operation_id=operation_id,
            cwd=cwd,
            command=command,
        )

        create_pty_operation_request.additional_properties = d
        return create_pty_operation_request

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
