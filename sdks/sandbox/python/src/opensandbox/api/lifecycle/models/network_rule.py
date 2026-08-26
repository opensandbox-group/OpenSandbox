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
from typing import Any, TypeVar, cast

from attrs import define as _attrs_define

from ..models.network_rule_action import NetworkRuleAction
from ..types import UNSET, Unset

T = TypeVar("T", bound="NetworkRule")


@_attrs_define
class NetworkRule:
    """
    Attributes:
        action (NetworkRuleAction): Whether to allow or deny matching targets.
        target (str | Unset): FQDN, wildcard domain (e.g., "example.com", "*.example.com"), IP
            address, or CIDR. May be omitted when `ports` is set (see below);
            otherwise required.
        ports (list[int] | Unset): Restricts this rule to specific TCP destination ports.

            - Omitted or empty: the rule is not port-scoped (existing behavior).
            - `target` omitted, `ports` set: the rule applies to these ports
              across all IPv4/IPv6 destinations.
            - `target` is an IP or CIDR, `ports` set: the rule applies to that
              destination only on these ports.
            - `target` is a domain: `ports` is rejected with a validation
              error. Domain rules are evaluated at the DNS layer, which has no
              port dimension; combining them is not supported yet.

            TCP only for now; UDP port scoping is not supported. A rule must
            set `target`, `ports`, or both. Max 256 ports per rule.
    """

    action: NetworkRuleAction
    target: str | Unset = UNSET
    ports: list[int] | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        action = self.action.value

        target = self.target

        ports: list[int] | Unset = UNSET
        if not isinstance(self.ports, Unset):
            ports = self.ports

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "action": action,
            }
        )
        if target is not UNSET:
            field_dict["target"] = target
        if ports is not UNSET:
            field_dict["ports"] = ports

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        action = NetworkRuleAction(d.pop("action"))

        target = d.pop("target", UNSET)

        ports = cast(list[int], d.pop("ports", UNSET))

        network_rule = cls(
            action=action,
            target=target,
            ports=ports,
        )

        return network_rule
