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

from ..models.allocation_summary_mode import AllocationSummaryMode
from ..models.allocation_summary_state import AllocationSummaryState

T = TypeVar("T", bound="AllocationSummary")


@_attrs_define
class AllocationSummary:
    """Public summary of a confirmed active pool allocation.

    Attributes:
        mode (AllocationSummaryMode): Allocation mode.
        pool_ref (str): Concrete pool reference currently allocated.
        state (AllocationSummaryState): Current confirmed allocation state.
    """

    mode: AllocationSummaryMode
    pool_ref: str
    state: AllocationSummaryState

    def to_dict(self) -> dict[str, Any]:
        mode = self.mode.value

        pool_ref = self.pool_ref

        state = self.state.value

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "mode": mode,
                "poolRef": pool_ref,
                "state": state,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        mode = AllocationSummaryMode(d.pop("mode"))

        pool_ref = d.pop("poolRef")

        state = AllocationSummaryState(d.pop("state"))

        allocation_summary = cls(
            mode=mode,
            pool_ref=pool_ref,
            state=state,
        )

        return allocation_summary
