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
from typing import TYPE_CHECKING, Any, TypeVar, cast

from attrs import define as _attrs_define

from ..models.create_fsb_template_request_format import CreateFsbTemplateRequestFormat
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.create_fsb_template_request_metadata import CreateFsbTemplateRequestMetadata
    from ..models.fsb_template_readiness import FsbTemplateReadiness
    from ..models.resource_limits import ResourceLimits


T = TypeVar("T", bound="CreateFsbTemplateRequest")


@_attrs_define
class CreateFsbTemplateRequest:
    """Request to create a Fast Sandbox template: a fast-sandbox golden-image build
    The server persists the build intent, projects it onto a
    SandboxTemplate CRD, and reports the asynchronous build through the
    template status. Kernel, execd and guest init are server-side build
    inputs supplied from the `[fsb]` configuration, not client fields.

        Attributes:
            image (str): Source OCI image reference the golden image is built from.
            publish (str): S3-compatible publish target for the built artifacts,
                e.g. `s3://bucket/publish`.
            resource_limits (ResourceLimits | Unset): Runtime resource constraints as key-value pairs. Similar to Kubernetes
                resource specifications,
                allows flexible definition of resource limits. Common resource types include:
                - `cpu`: CPU allocation in millicores (e.g., "250m" for 0.25 CPU cores)
                - `memory`: Memory allocation in bytes or human-readable format (e.g., "512Mi", "1Gi")
                - `gpu`: Number of GPU devices (e.g., "1")

                New resource types can be added without API changes.
                 Example: {'cpu': '500m', 'memory': '512Mi', 'gpu': '1'}.
            entrypoint (list[str] | Unset): Guest business command (argv); empty defaults to
                `["tail", "-f", "/dev/null"]`.
            metadata (CreateFsbTemplateRequestMetadata | Unset): Custom key-value metadata for management, filtering, and
                tagging.
            readiness (FsbTemplateReadiness | Unset): Build-side readiness gate for a fsb template.
            format_ (CreateFsbTemplateRequestFormat | Unset): Storage encoding of the produced snapshot set. Default:
                CreateFsbTemplateRequestFormat.OVERLAYBD.
    """

    image: str
    publish: str
    resource_limits: ResourceLimits | Unset = UNSET
    entrypoint: list[str] | Unset = UNSET
    metadata: CreateFsbTemplateRequestMetadata | Unset = UNSET
    readiness: FsbTemplateReadiness | Unset = UNSET
    format_: CreateFsbTemplateRequestFormat | Unset = CreateFsbTemplateRequestFormat.OVERLAYBD

    def to_dict(self) -> dict[str, Any]:
        image = self.image

        publish = self.publish

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

        format_: str | Unset = UNSET
        if not isinstance(self.format_, Unset):
            format_ = self.format_.value

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "image": image,
                "publish": publish,
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
        if format_ is not UNSET:
            field_dict["format"] = format_

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.create_fsb_template_request_metadata import CreateFsbTemplateRequestMetadata
        from ..models.fsb_template_readiness import FsbTemplateReadiness
        from ..models.resource_limits import ResourceLimits

        d = dict(src_dict)
        image = d.pop("image")

        publish = d.pop("publish")

        _resource_limits = d.pop("resourceLimits", UNSET)
        resource_limits: ResourceLimits | Unset
        if isinstance(_resource_limits, Unset):
            resource_limits = UNSET
        else:
            resource_limits = ResourceLimits.from_dict(_resource_limits)

        entrypoint = cast(list[str], d.pop("entrypoint", UNSET))

        _metadata = d.pop("metadata", UNSET)
        metadata: CreateFsbTemplateRequestMetadata | Unset
        if isinstance(_metadata, Unset):
            metadata = UNSET
        else:
            metadata = CreateFsbTemplateRequestMetadata.from_dict(_metadata)

        _readiness = d.pop("readiness", UNSET)
        readiness: FsbTemplateReadiness | Unset
        if isinstance(_readiness, Unset):
            readiness = UNSET
        else:
            readiness = FsbTemplateReadiness.from_dict(_readiness)

        _format_ = d.pop("format", UNSET)
        format_: CreateFsbTemplateRequestFormat | Unset
        if isinstance(_format_, Unset):
            format_ = UNSET
        else:
            format_ = CreateFsbTemplateRequestFormat(_format_)

        create_fsb_template_request = cls(
            image=image,
            publish=publish,
            resource_limits=resource_limits,
            entrypoint=entrypoint,
            metadata=metadata,
            readiness=readiness,
            format_=format_,
        )

        return create_fsb_template_request
