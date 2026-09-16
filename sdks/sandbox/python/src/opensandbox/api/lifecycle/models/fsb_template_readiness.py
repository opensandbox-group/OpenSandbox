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

from ..types import UNSET, Unset

T = TypeVar("T", bound="FsbTemplateReadiness")


@_attrs_define
class FsbTemplateReadiness:
    """Build-side readiness gate for a fsb template.

    Attributes:
        probe (str | Unset): Readiness probe checked first during the golden-image build;
            e.g. `tcp://127.0.0.1:44772` or `cmd://<command>`.
        warmup_seconds (int | Unset): Fallback warmup window in seconds (default 60).
    """

    probe: str | Unset = UNSET
    warmup_seconds: int | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        probe = self.probe

        warmup_seconds = self.warmup_seconds

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if probe is not UNSET:
            field_dict["probe"] = probe
        if warmup_seconds is not UNSET:
            field_dict["warmupSeconds"] = warmup_seconds

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        probe = d.pop("probe", UNSET)

        warmup_seconds = d.pop("warmupSeconds", UNSET)

        fsb_template_readiness = cls(
            probe=probe,
            warmup_seconds=warmup_seconds,
        )

        return fsb_template_readiness
