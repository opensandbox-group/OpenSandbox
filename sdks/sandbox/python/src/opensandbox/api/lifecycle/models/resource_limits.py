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

T = TypeVar("T", bound="ResourceLimits")


@_attrs_define
class ResourceLimits:
    """Runtime resource constraints as key-value pairs. Similar to Kubernetes resource specifications,
    allows flexible definition of resource limits. Common resource types include:
    - `cpu`: CPU allocation in millicores (e.g., "250m" for 0.25 CPU cores)
    - `memory`: Memory allocation in bytes or human-readable format (e.g., "512Mi", "1Gi")
    - `gpu`: Number of GPU devices (e.g., "1")
    - `disk`: Writable-layer disk quota (e.g., "10G", "512M"); `storage` and
      `ephemeral-storage` are accepted as aliases. On the Docker runtime this
      bounds only the container's writable layer via HostConfig.StorageOpt —
      bind mounts, named volumes, and read-only image layers are not covered —
      and requires a quota-capable storage driver (for example overlay2 on XFS
      with pquota); the create request fails with INVALID_PARAMETER if the
      daemon cannot enforce it. On the Windows profile this maps to the
      sandbox disk size.

    New resource types can be added without API changes.

        Example:
            {'cpu': '500m', 'memory': '512Mi', 'gpu': '1'}

    """

    additional_properties: dict[str, str] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        resource_limits = cls()

        resource_limits.additional_properties = d
        return resource_limits

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
