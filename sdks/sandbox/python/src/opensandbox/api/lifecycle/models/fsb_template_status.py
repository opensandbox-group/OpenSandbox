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

from ..models.fsb_template_status_phase import FsbTemplateStatusPhase
from ..types import UNSET, Unset

T = TypeVar("T", bound="FsbTemplateStatus")


@_attrs_define
class FsbTemplateStatus:
    """Status of a fsb template build.

    Attributes:
        phase (FsbTemplateStatusPhase): Build lifecycle phase.
        manifest_ref (str | Unset): S3 manifest reference of the published artifacts; present when
            Succeeded.
        message (str | Unset): Failure reason when phase is Failed.
    """

    phase: FsbTemplateStatusPhase
    manifest_ref: str | Unset = UNSET
    message: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        phase = self.phase.value

        manifest_ref = self.manifest_ref

        message = self.message

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "phase": phase,
            }
        )
        if manifest_ref is not UNSET:
            field_dict["manifestRef"] = manifest_ref
        if message is not UNSET:
            field_dict["message"] = message

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        phase = FsbTemplateStatusPhase(d.pop("phase"))

        manifest_ref = d.pop("manifestRef", UNSET)

        message = d.pop("message", UNSET)

        fsb_template_status = cls(
            phase=phase,
            manifest_ref=manifest_ref,
            message=message,
        )

        return fsb_template_status
