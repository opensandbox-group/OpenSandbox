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

from ..models.hardening_layer_state_state import HardeningLayerStateState
from ..types import UNSET, Unset

T = TypeVar("T", bound="HardeningLayerState")


@_attrs_define
class HardeningLayerState:
    """Whether one hardening layer is actually enforced. state is "active" | "disabled" (not configured) | "degraded"
    (configured but a prerequisite is missing) | "unsupported" (kernel/build cannot provide it). message gives the
    concrete reason whenever state is not active.

        Attributes:
            state (HardeningLayerStateState | Unset):
            message (str | Unset):
    """

    state: HardeningLayerStateState | Unset = UNSET
    message: str | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        state: str | Unset = UNSET
        if not isinstance(self.state, Unset):
            state = self.state.value

        message = self.message

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update({})
        if state is not UNSET:
            field_dict["state"] = state
        if message is not UNSET:
            field_dict["message"] = message

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        _state = d.pop("state", UNSET)
        state: HardeningLayerStateState | Unset
        if isinstance(_state, Unset):
            state = UNSET
        else:
            state = HardeningLayerStateState(_state)

        message = d.pop("message", UNSET)

        hardening_layer_state = cls(
            state=state,
            message=message,
        )

        hardening_layer_state.additional_properties = d
        return hardening_layer_state

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
