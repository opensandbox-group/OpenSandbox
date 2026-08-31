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

from ..models.metrics_event_event_type import MetricsEventEventType
from ..types import UNSET, Unset

T = TypeVar("T", bound="MetricsEvent")


@_attrs_define
class MetricsEvent:
    """SDK-reported metrics event. Phase 1 covers sandbox creation latency from
    the start of create until readiness succeeds or creation fails.

    SDK language and package version are identified via the request
    `User-Agent` header (for example `OpenSandbox-Go-SDK/0.1.0`), not body fields.

        Attributes:
            event_type (MetricsEventEventType): Metric event type
            create_duration_ms (int): Wall-clock duration in milliseconds from create start to ready or failure
            success (bool): Whether create + readiness completed successfully
            sandbox_id (str | Unset): Sandbox identifier when available (may be omitted if create failed before an ID was
                assigned)
            image (str | Unset): Container image URI or snapshot startup source label
    """

    event_type: MetricsEventEventType
    create_duration_ms: int
    success: bool
    sandbox_id: str | Unset = UNSET
    image: str | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        event_type = self.event_type.value

        create_duration_ms = self.create_duration_ms

        success = self.success

        sandbox_id = self.sandbox_id

        image = self.image

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "eventType": event_type,
                "createDurationMs": create_duration_ms,
                "success": success,
            }
        )
        if sandbox_id is not UNSET:
            field_dict["sandboxId"] = sandbox_id
        if image is not UNSET:
            field_dict["image"] = image

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        event_type = MetricsEventEventType(d.pop("eventType"))

        create_duration_ms = d.pop("createDurationMs")

        success = d.pop("success")

        sandbox_id = d.pop("sandboxId", UNSET)

        image = d.pop("image", UNSET)

        metrics_event = cls(
            event_type=event_type,
            create_duration_ms=create_duration_ms,
            success=success,
            sandbox_id=sandbox_id,
            image=image,
        )

        metrics_event.additional_properties = d
        return metrics_event

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
