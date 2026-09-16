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
"""
Template model converter utilities.

Provides conversion functions between generated lifecycle API models and
public domain models for fsb template management.
"""

from typing import Literal, cast

from opensandbox.api.lifecycle.models.create_fsb_template_request import (
    CreateFsbTemplateRequest as ApiCreateTemplateRequest,
)
from opensandbox.api.lifecycle.models.create_fsb_template_request_format import (
    CreateFsbTemplateRequestFormat,
)
from opensandbox.api.lifecycle.models.create_fsb_template_request_metadata import (
    CreateFsbTemplateRequestMetadata,
)
from opensandbox.api.lifecycle.models.fsb_template import FsbTemplate as ApiTemplate
from opensandbox.api.lifecycle.models.fsb_template_readiness import (
    FsbTemplateReadiness as ApiTemplateReadiness,
)
from opensandbox.api.lifecycle.models.list_fsb_templates_response import (
    ListFsbTemplatesResponse as ApiListTemplatesResponse,
)
from opensandbox.api.lifecycle.models.resource_limits import ResourceLimits
from opensandbox.api.lifecycle.types import UNSET, Unset
from opensandbox.models.sandboxes import PaginationInfo
from opensandbox.models.templates import (
    CreateTemplateRequest,
    PagedTemplateInfos,
    TemplateInfo,
    TemplateReadiness,
    TemplateStatus,
)


class TemplateModelConverter:
    """
    Template model converter utilities.

    Converts between openapi-python-client generated attrs models and the
    public pydantic domain models, keeping the generated layer out of the
    SDK public surface.
    """

    @staticmethod
    def _metadata_to_dict(value: object) -> dict[str, str] | None:
        if isinstance(value, Unset):
            return None
        if hasattr(value, "additional_properties"):
            props = getattr(value, "additional_properties", None)
            if isinstance(props, dict):
                return dict(props)
        if isinstance(value, dict):
            return value
        return None

    @staticmethod
    def to_api_create_template_request(
        request: CreateTemplateRequest,
    ) -> ApiCreateTemplateRequest:
        """Convert domain CreateTemplateRequest to the generated API request."""
        api_metadata = (
            CreateFsbTemplateRequestMetadata.from_dict(request.metadata)
            if request.metadata
            else UNSET
        )
        api_readiness = (
            ApiTemplateReadiness(
                probe=(
                    request.readiness.probe
                    if request.readiness.probe is not None
                    else UNSET
                ),
                warmup_seconds=(
                    request.readiness.warmup_seconds
                    if request.readiness.warmup_seconds is not None
                    else UNSET
                ),
            )
            if request.readiness is not None
            else UNSET
        )
        return ApiCreateTemplateRequest(
            image=request.image,
            publish=request.publish,
            resource_limits=(
                ResourceLimits.from_dict(request.resource_limits)
                if request.resource_limits
                else UNSET
            ),
            entrypoint=(
                request.entrypoint if request.entrypoint is not None else UNSET
            ),
            metadata=api_metadata,
            readiness=api_readiness,
            format_=(
                CreateFsbTemplateRequestFormat(request.format)
                if request.format is not None
                else UNSET
            ),
        )

    @staticmethod
    def to_template_info(api_template: ApiTemplate) -> TemplateInfo:
        """Convert generated FsbTemplate to domain TemplateInfo."""
        api_status = api_template.status
        manifest_ref = api_status.manifest_ref
        if isinstance(manifest_ref, Unset):
            manifest_ref = None
        message = api_status.message
        if isinstance(message, Unset):
            message = None

        entrypoint = api_template.entrypoint
        if isinstance(entrypoint, Unset):
            entrypoint = None

        readiness = None
        if not isinstance(api_template.readiness, Unset):
            probe = api_template.readiness.probe
            if isinstance(probe, Unset):
                probe = None
            warmup_seconds = api_template.readiness.warmup_seconds
            if isinstance(warmup_seconds, Unset):
                warmup_seconds = None
            readiness = TemplateReadiness(
                probe=probe,
                warmupSeconds=warmup_seconds,
            )

        return TemplateInfo(
            templateId=api_template.template_id,
            image=api_template.image,
            publish=api_template.publish,
            format=cast(
                Literal["native", "overlaybd"],
                str(getattr(api_template.format_, "value", api_template.format_)),
            ),
            status=TemplateStatus(
                phase=str(
                    getattr(api_status.phase, "value", api_status.phase)
                ),
                manifestRef=manifest_ref,
                message=message,
            ),
            created_at=api_template.created_at,
            updated_at=api_template.updated_at,
            resourceLimits=(
                dict(api_template.resource_limits.additional_properties)
                if not isinstance(api_template.resource_limits, Unset)
                else None
            ),
            entrypoint=entrypoint,
            metadata=TemplateModelConverter._metadata_to_dict(
                api_template.metadata
            ),
            readiness=readiness,
        )

    @staticmethod
    def to_paged_template_infos(
        api_response: ApiListTemplatesResponse,
    ) -> PagedTemplateInfos:
        """Convert generated ListFsbTemplatesResponse to domain PagedTemplateInfos."""
        items = api_response.items if hasattr(api_response, "items") else []
        api_pagination = api_response.pagination

        return PagedTemplateInfos(
            template_infos=[
                TemplateModelConverter.to_template_info(t) for t in items
            ],
            pagination=PaginationInfo(
                page=api_pagination.page or 1,
                page_size=api_pagination.page_size or 10,
                total_pages=api_pagination.total_pages or 0,
                total_items=api_pagination.total_items or 0,
                has_next_page=api_pagination.has_next_page or False,
            ),
        )
