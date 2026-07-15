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

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.network_policy import NetworkPolicy


T = TypeVar("T", bound="PolicyStatusResponse")


@_attrs_define
class PolicyStatusResponse:
    """
    Attributes:
        status (str | Unset): Operation status reported by the sidecar. Example: ok.
        mode (str | Unset): Derived runtime mode for the current policy. Example: deny_all.
        enforcement_mode (str | Unset): Egress sidecar enforcement backend mode. Example: dns.
        reason (str | Unset): Optional human-readable reason when the sidecar returns extra context.
        policy (NetworkPolicy | Unset): Egress network policy matching the sidecar `/policy` request body.
            If `defaultAction` is omitted, the sidecar defaults to "deny"; passing an empty
            object or null results in allow-all behavior at startup.
    """

    status: str | Unset = UNSET
    mode: str | Unset = UNSET
    enforcement_mode: str | Unset = UNSET
    reason: str | Unset = UNSET
    policy: NetworkPolicy | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        status = self.status

        mode = self.mode

        enforcement_mode = self.enforcement_mode

        reason = self.reason

        policy: dict[str, Any] | Unset = UNSET
        if not isinstance(self.policy, Unset):
            policy = self.policy.to_dict()

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if status is not UNSET:
            field_dict["status"] = status
        if mode is not UNSET:
            field_dict["mode"] = mode
        if enforcement_mode is not UNSET:
            field_dict["enforcementMode"] = enforcement_mode
        if reason is not UNSET:
            field_dict["reason"] = reason
        if policy is not UNSET:
            field_dict["policy"] = policy

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.network_policy import NetworkPolicy

        d = dict(src_dict)
        status = d.pop("status", UNSET)

        mode = d.pop("mode", UNSET)

        enforcement_mode = d.pop("enforcementMode", UNSET)

        reason = d.pop("reason", UNSET)

        _policy = d.pop("policy", UNSET)
        policy: NetworkPolicy | Unset
        if isinstance(_policy, Unset):
            policy = UNSET
        else:
            policy = NetworkPolicy.from_dict(_policy)

        policy_status_response = cls(
            status=status,
            mode=mode,
            enforcement_mode=enforcement_mode,
            reason=reason,
            policy=policy,
        )

        return policy_status_response
