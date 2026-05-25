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

from ..models.network_policy_default_action import NetworkPolicyDefaultAction
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.network_rule import NetworkRule


T = TypeVar("T", bound="NetworkPolicy")


@_attrs_define
class NetworkPolicy:
    """Egress network policy matching the sidecar `/policy` request body.
    If `defaultAction` is omitted, the sidecar defaults to "deny"; passing an empty
    object or null results in allow-all behavior at startup.

        Attributes:
            default_action (NetworkPolicyDefaultAction | Unset): Default action when no egress rule matches. Defaults to
                "deny".
            egress (list[NetworkRule] | Unset): List of egress rules evaluated in order.
    """

    default_action: NetworkPolicyDefaultAction | Unset = UNSET
    egress: list[NetworkRule] | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        default_action: str | Unset = UNSET
        if not isinstance(self.default_action, Unset):
            default_action = self.default_action.value

        egress: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.egress, Unset):
            egress = []
            for egress_item_data in self.egress:
                egress_item = egress_item_data.to_dict()
                egress.append(egress_item)

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if default_action is not UNSET:
            field_dict["defaultAction"] = default_action
        if egress is not UNSET:
            field_dict["egress"] = egress

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.network_rule import NetworkRule

        d = dict(src_dict)
        _default_action = d.pop("defaultAction", UNSET)
        default_action: NetworkPolicyDefaultAction | Unset
        if isinstance(_default_action, Unset):
            default_action = UNSET
        else:
            default_action = NetworkPolicyDefaultAction(_default_action)

        _egress = d.pop("egress", UNSET)
        egress: list[NetworkRule] | Unset = UNSET
        if _egress is not UNSET:
            egress = []
            for egress_item_data in _egress:
                egress_item = NetworkRule.from_dict(egress_item_data)

                egress.append(egress_item)

        network_policy = cls(
            default_action=default_action,
            egress=egress,
        )

        return network_policy
