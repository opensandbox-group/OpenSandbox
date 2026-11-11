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

T = TypeVar("T", bound="ResourceRequests")


@_attrs_define
class ResourceRequests:
    """Resource reservations for the sandbox, as key-value pairs in Kubernetes
    resource specification syntax (same key space as `resourceLimits`):
    - `cpu`: CPU reservation (e.g., "250m" for 0.25 CPU cores)
    - `memory`: Memory reservation (e.g., "512Mi", "1Gi")

    On Kubernetes-based runtimes these become the sandbox container's
    `requests` — scheduling guarantees, not hard caps. This field is
    ignored by the Docker runtime, which reads only `resourceLimits`, so
    a `disk` reservation here does not create a writable-layer quota.

        Example:
            {'cpu': '500m', 'memory': '512Mi'}

    """

    additional_properties: dict[str, str] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        resource_requests = cls()

        resource_requests.additional_properties = d
        return resource_requests

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
