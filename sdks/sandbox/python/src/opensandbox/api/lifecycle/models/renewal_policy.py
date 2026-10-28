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

T = TypeVar("T", bound="RenewalPolicy")


@_attrs_define
class RenewalPolicy:
    """Renew-on-access behavior declared by the sandbox (OSEP-0009). The
    policy is desired control-plane state: when the ingress gateway or
    the server proxy observes traffic for the sandbox, the server extends
    the sandbox expiration without client involvement. The policy never
    reaches the guest.

    Setting the policy has no effect unless the server operator enabled
    the renew-intent integration (`[renew_intent]`). A missing policy
    means the sandbox never auto-renews.

        Attributes:
            extend_seconds (int): Extension window X in seconds for each access-driven renewal:
                the expiration is moved to `max(now + X, current expiresAt)`.
                Must be between 300 (5 minutes) and 86400 (24 hours) inclusive.
    """

    extend_seconds: int

    def to_dict(self) -> dict[str, Any]:
        extend_seconds = self.extend_seconds

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "extendSeconds": extend_seconds,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        extend_seconds = d.pop("extendSeconds")

        renewal_policy = cls(
            extend_seconds=extend_seconds,
        )

        return renewal_policy
