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

T = TypeVar("T", bound="Metrics")


@_attrs_define
class Metrics:
    """System resource usage metrics

    Attributes:
        cpu_count (float): Number of CPU cores visible to the process (GOMAXPROCS, may reflect cgroup CPU quota)
            Example: 4.0.
        cpu_used_pct (float): CPU usage percentage Example: 45.5.
        mem_total_mib (float): Total memory in MiB Example: 8192.0.
        mem_used_mib (float): Used memory in MiB Example: 4096.0.
        timestamp (int): Timestamp when metrics were collected (Unix milliseconds) Example: 1700000000000.
    """

    cpu_count: float
    cpu_used_pct: float
    mem_total_mib: float
    mem_used_mib: float
    timestamp: int
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        cpu_count = self.cpu_count

        cpu_used_pct = self.cpu_used_pct

        mem_total_mib = self.mem_total_mib

        mem_used_mib = self.mem_used_mib

        timestamp = self.timestamp

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "cpu_count": cpu_count,
                "cpu_used_pct": cpu_used_pct,
                "mem_total_mib": mem_total_mib,
                "mem_used_mib": mem_used_mib,
                "timestamp": timestamp,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        cpu_count = d.pop("cpu_count")

        cpu_used_pct = d.pop("cpu_used_pct")

        mem_total_mib = d.pop("mem_total_mib")

        mem_used_mib = d.pop("mem_used_mib")

        timestamp = d.pop("timestamp")

        metrics = cls(
            cpu_count=cpu_count,
            cpu_used_pct=cpu_used_pct,
            mem_total_mib=mem_total_mib,
            mem_used_mib=mem_used_mib,
            timestamp=timestamp,
        )

        metrics.additional_properties = d
        return metrics

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
