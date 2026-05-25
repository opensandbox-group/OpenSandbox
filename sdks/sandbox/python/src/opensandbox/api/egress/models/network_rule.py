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

from ..models.network_rule_action import NetworkRuleAction

T = TypeVar("T", bound="NetworkRule")


@_attrs_define
class NetworkRule:
    """
    Attributes:
        action (NetworkRuleAction): Whether to allow or deny matching targets.
        target (str): FQDN or wildcard domain (e.g., "example.com", "*.example.com").
            IP/CIDR not yet supported in the egress MVP.
    """

    action: NetworkRuleAction
    target: str

    def to_dict(self) -> dict[str, Any]:
        action = self.action.value

        target = self.target

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "action": action,
                "target": target,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        action = NetworkRuleAction(d.pop("action"))

        target = d.pop("target")

        network_rule = cls(
            action=action,
            target=target,
        )

        return network_rule
