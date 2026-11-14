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
from attrs import field as _attrs_field

T = TypeVar("T", bound="FsbTemplateResourceLimits")


@_attrs_define
class FsbTemplateResourceLimits:
    """Guest machine sizing for a Fast Sandbox template, as key-value pairs:
    - `cpu`: guest vCPUs (e.g., "1")
    - `memory`: guest memory (e.g., "512Mi")
    - `disk`: logical size of the guest rootfs (e.g., "2Gi"); the artifact
      set is stored and P2P-pulled at this size, so keep it just above the
      expanded source image.

    Defaults when omitted: cpu `1`, memory `512Mi`, disk `2Gi`.
    Only these three keys are accepted: `storage` and `ephemeral-storage`
    are sandbox writable-layer quota aliases and are not template keys —
    the request fails with HTTP 400 if they are set here.

        Example:
            {'cpu': '1', 'memory': '512Mi', 'disk': '2Gi'}

    """

    additional_properties: dict[str, str] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        fsb_template_resource_limits = cls()

        fsb_template_resource_limits.additional_properties = d
        return fsb_template_resource_limits

    @property
    def additional_keys(self) -> list[str]:
        return list(self.additional_properties.keys())

    def __getitem__(self, key: str) -> str:
        return self.additional_properties[key]

    def __setitem__(self, key: str, value: str) -> None:
        self.additional_properties[key] = value

    def __delitem__(self, key: str) -> None:
        del self.additional_properties[key]

    def __contains__(self, key: str) -> bool:
        return key in self.additional_properties
