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

"""
API routes for fsb template management.

Templates declare fast-sandbox golden-image builds; the server persists the
catalog, projects each row onto a SandboxTemplate CRD, and reports the
asynchronous build status. Available on the kubernetes runtime.
"""

from typing import Optional

from fastapi import APIRouter, Query, status
from fastapi.exceptions import HTTPException
from fastapi.responses import Response

from opensandbox_server.api.schema import (
    CreateFsbTemplateRequest,
    ErrorResponse,
    FsbTemplate,
    ListFsbTemplatesResponse,
    PaginationInfo,
)
from opensandbox_server.config import get_config
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.templates.template_service import (
    FsbTemplateService,
    template_to_response,
    total_pages,
)

router = APIRouter(tags=["Templates"])

_TEMPLATE_NOT_K8S_DETAIL = {
    "code": SandboxErrorCodes.FSB_UNSUPPORTED,
    "message": "Template management requires the kubernetes runtime.",
}

_service: Optional[FsbTemplateService] = None


def _get_template_service() -> FsbTemplateService:
    """Lazily create the service, raising 501 for non-Kubernetes runtimes.

    Templates project onto fast-sandbox CRDs through the Kubernetes client,
    available on the kubernetes runtime (fsb sandboxes coexist with the
    container-sandbox workload provider).
    """
    global _service
    if _service is not None:
        return _service
    config = get_config()
    if config.runtime.type == "docker" or config.kubernetes is None:
        raise HTTPException(
            status_code=status.HTTP_501_NOT_IMPLEMENTED,
            detail=_TEMPLATE_NOT_K8S_DETAIL,
        )
    _service = FsbTemplateService(config)
    # Watch-driven convergence: DB rows track the CRD build phase even when
    # nobody polls the read API.
    _service.start_background_sync()
    return _service


def close_template_service() -> None:
    global _service
    if _service is not None:
        _service.close()
        _service = None


@router.post(
    "/templates",
    response_model=FsbTemplate,
    response_model_exclude_none=True,
    status_code=status.HTTP_201_CREATED,
    responses={
        201: {"description": "Template build accepted; phase starts at Pending"},
        400: {"model": ErrorResponse, "description": "The request was invalid or malformed"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        409: {"model": ErrorResponse, "description": "A template build with the same name already exists"},
        501: {"model": ErrorResponse, "description": "Template management is not supported in this runtime"},
        503: {"model": ErrorResponse, "description": "SandboxTemplate CRDs are temporarily unavailable"},
    },
)
def create_template(request: CreateFsbTemplateRequest) -> FsbTemplate:
    """
    Create a fsb template (golden-image build).

    The build runs asynchronously in fast-sandbox: the response carries
    ``phase: Pending`` and a ``Location`` header; poll
    ``GET /templates/{templateId}`` until ``Succeeded`` (or ``Failed``).
    Only ``Succeeded`` templates can be used for template-based sandbox
    creation. Kernel, execd and guest init are server-side build inputs
    supplied from the ``[kubernetes]`` configuration.
    """
    record = _get_template_service().create_template(request)
    return template_to_response(record)


@router.get(
    "/templates",
    response_model=ListFsbTemplatesResponse,
    response_model_exclude_none=True,
    responses={
        200: {"description": "Paginated collection of templates"},
        400: {"model": ErrorResponse, "description": "The request was invalid or malformed"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        501: {"model": ErrorResponse, "description": "Template management is not supported in this runtime"},
    },
)
def list_templates(
    page: int = Query(1, ge=1, description="Page number"),
    page_size: int = Query(20, ge=1, le=200, alias="pageSize", description="Items per page"),
    metadata: Optional[str] = Query(
        None,
        description="Metadata filter as URL-encoded key-value pairs with AND logic (e.g. project%3DApollo%26env%3Dprod)",
    ),
) -> ListFsbTemplatesResponse:
    """
    List the current tenant's templates.

    Results are scoped to the tenant's fast-sandbox namespace; other
    tenants' templates are never returned.
    """
    from urllib.parse import parse_qsl

    metadata_filter = dict(parse_qsl(metadata)) if metadata else None
    items, total_items = _get_template_service().list_templates(
        metadata=metadata_filter,
        page=page,
        page_size=page_size,
    )
    return ListFsbTemplatesResponse(
        items=[template_to_response(record) for record in items],
        pagination=PaginationInfo(
            page=page,
            pageSize=page_size,
            totalItems=total_items,
            totalPages=total_pages(total_items, page_size),
            hasNextPage=page < total_pages(total_items, page_size),
        ),
    )


@router.get(
    "/templates/{template_id}",
    response_model=FsbTemplate,
    response_model_exclude_none=True,
    responses={
        200: {"description": "Template status and artifact references"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        404: {"model": ErrorResponse, "description": "Unknown template for this tenant"},
        501: {"model": ErrorResponse, "description": "Template management is not supported in this runtime"},
    },
)
def get_template(template_id: str) -> FsbTemplate:
    """
    Get one template.

    The build status is lazily synced from the SandboxTemplate CRD, so the
    response reflects the latest fast-sandbox build phase.
    """
    record = _get_template_service().get_template(template_id)
    return template_to_response(record)


@router.delete(
    "/templates/{template_id}",
    status_code=status.HTTP_204_NO_CONTENT,
    responses={
        204: {"description": "Template deleted; running sandboxes are unaffected"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        404: {"model": ErrorResponse, "description": "Unknown template for this tenant"},
        501: {"model": ErrorResponse, "description": "Template management is not supported in this runtime"},
    },
)
def delete_template(template_id: str) -> Response:
    """
    Delete a template.

    Removes the catalog row and the SandboxTemplate CRD. Sandboxes already
    created from the template are unaffected: their Sandbox CRs hold their
    own artifact references.
    """
    _get_template_service().delete_template(template_id)
    return Response(status_code=status.HTTP_204_NO_CONTENT)
