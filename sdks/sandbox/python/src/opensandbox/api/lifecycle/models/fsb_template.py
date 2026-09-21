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

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar, cast

from attrs import define as _attrs_define
from dateutil.parser import isoparse

from ..models.fsb_template_format import FsbTemplateFormat
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.fsb_template_metadata import FsbTemplateMetadata
    from ..models.fsb_template_readiness import FsbTemplateReadiness
    from ..models.fsb_template_status import FsbTemplateStatus
    from ..models.resource_limits import ResourceLimits


T = TypeVar("T", bound="FsbTemplate")


@_attrs_define
class FsbTemplate:
    """A fsb template: a golden image whose build is declared and executed
    by fast-sandbox.

        Attributes:
            template_id (str): Server-generated template ID (`tpl_<uuid>`).
            image (str): Source OCI image reference.
            publish (str): S3-compatible publish target.
            format_ (FsbTemplateFormat): Snapshot storage encoding.
            status (FsbTemplateStatus): Status of a fsb template build.
            created_at (datetime.datetime): Creation timestamp (RFC 3339 UTC).
            updated_at (datetime.datetime): Last update timestamp (RFC 3339 UTC).
            resource_limits (ResourceLimits | Unset): Runtime resource constraints as key-value pairs. Similar to Kubernetes
                resource specifications,
                allows flexible definition of resource limits. Common resource types include:
                - `cpu`: CPU allocation in millicores (e.g., "250m" for 0.25 CPU cores)
                - `memory`: Memory allocation in bytes or human-readable format (e.g., "512Mi", "1Gi")
                - `gpu`: Number of GPU devices (e.g., "1")

                New resource types can be added without API changes.
                 Example: {'cpu': '500m', 'memory': '512Mi', 'gpu': '1'}.
            entrypoint (list[str] | Unset): Guest business command (argv).
            metadata (FsbTemplateMetadata | Unset): Custom metadata from the creation request.
            readiness (FsbTemplateReadiness | Unset): Build-side readiness gate for a fsb template.
    """

    template_id: str
    image: str
    publish: str
    format_: FsbTemplateFormat
    status: FsbTemplateStatus
    created_at: datetime.datetime
    updated_at: datetime.datetime
    resource_limits: ResourceLimits | Unset = UNSET
    entrypoint: list[str] | Unset = UNSET
    metadata: FsbTemplateMetadata | Unset = UNSET
    readiness: FsbTemplateReadiness | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        template_id = self.template_id

        image = self.image

        publish = self.publish

        format_ = self.format_.value

        status = self.status.to_dict()

        created_at = self.created_at.isoformat()

        updated_at = self.updated_at.isoformat()

        resource_limits: dict[str, Any] | Unset = UNSET
        if not isinstance(self.resource_limits, Unset):
            resource_limits = self.resource_limits.to_dict()

        entrypoint: list[str] | Unset = UNSET
        if not isinstance(self.entrypoint, Unset):
            entrypoint = self.entrypoint

        metadata: dict[str, Any] | Unset = UNSET
        if not isinstance(self.metadata, Unset):
            metadata = self.metadata.to_dict()

        readiness: dict[str, Any] | Unset = UNSET
        if not isinstance(self.readiness, Unset):
            readiness = self.readiness.to_dict()

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "templateId": template_id,
                "image": image,
                "publish": publish,
                "format": format_,
                "status": status,
                "createdAt": created_at,
                "updatedAt": updated_at,
            }
        )
        if resource_limits is not UNSET:
            field_dict["resourceLimits"] = resource_limits
        if entrypoint is not UNSET:
            field_dict["entrypoint"] = entrypoint
        if metadata is not UNSET:
            field_dict["metadata"] = metadata
        if readiness is not UNSET:
            field_dict["readiness"] = readiness

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.fsb_template_metadata import FsbTemplateMetadata
        from ..models.fsb_template_readiness import FsbTemplateReadiness
        from ..models.fsb_template_status import FsbTemplateStatus
        from ..models.resource_limits import ResourceLimits

        d = dict(src_dict)
        template_id = d.pop("templateId")

        image = d.pop("image")

        publish = d.pop("publish")

        format_ = FsbTemplateFormat(d.pop("format"))

        status = FsbTemplateStatus.from_dict(d.pop("status"))

        created_at = isoparse(d.pop("createdAt"))

        updated_at = isoparse(d.pop("updatedAt"))

        _resource_limits = d.pop("resourceLimits", UNSET)
        resource_limits: ResourceLimits | Unset
        if isinstance(_resource_limits, Unset):
            resource_limits = UNSET
        else:
            resource_limits = ResourceLimits.from_dict(_resource_limits)

        entrypoint = cast(list[str], d.pop("entrypoint", UNSET))

        _metadata = d.pop("metadata", UNSET)
        metadata: FsbTemplateMetadata | Unset
        if isinstance(_metadata, Unset):
            metadata = UNSET
        else:
            metadata = FsbTemplateMetadata.from_dict(_metadata)

        _readiness = d.pop("readiness", UNSET)
        readiness: FsbTemplateReadiness | Unset
        if isinstance(_readiness, Unset):
            readiness = UNSET
        else:
            readiness = FsbTemplateReadiness.from_dict(_readiness)

        fsb_template = cls(
            template_id=template_id,
            image=image,
            publish=publish,
            format_=format_,
            status=status,
            created_at=created_at,
            updated_at=updated_at,
            resource_limits=resource_limits,
            entrypoint=entrypoint,
            metadata=metadata,
            readiness=readiness,
        )

        return fsb_template
