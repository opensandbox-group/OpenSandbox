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
"""
Template data models.

Models for fast-sandbox (fsb) golden-image template management. Templates
declare an asynchronous golden-image build; only ``Succeeded`` templates can
back template-based sandbox creation. Template management requires a
Kubernetes-backed runtime.
"""

from datetime import datetime
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, field_validator

from opensandbox.models.sandboxes import PaginationInfo


class TemplatePhase:
    """High-level lifecycle phase of a fsb template build.

    Known values:
        PENDING (str): Build accepted, not started yet.
        BUILDING (str): Golden-image build in progress.
        SUCCEEDED (str): Build finished; the template can back sandbox creation.
        FAILED (str): Build failed; see ``TemplateStatus.message``.

    The server may introduce new phases in future versions; clients should
    handle unknown string values gracefully.
    """

    PENDING = "Pending"
    BUILDING = "Building"
    SUCCEEDED = "Succeeded"
    FAILED = "Failed"

    @classmethod
    def values(cls) -> set[str]:
        """Returns a set of all known phase values."""
        return {
            v for k, v in cls.__dict__.items() if k.isupper() and not k.startswith("_")
        }


class TemplateReadiness(BaseModel):
    """Build-side readiness gate for a fsb template."""

    probe: str | None = Field(
        default=None,
        description=(
            "Readiness probe checked first during the golden-image build; "
            "e.g. 'tcp://127.0.0.1:44772' or 'cmd://<command>'."
        ),
    )
    warmup_seconds: int | None = Field(
        default=None,
        alias="warmupSeconds",
        description="Fallback warmup window in seconds (default 60).",
    )

    model_config = ConfigDict(populate_by_name=True)


class TemplateStatus(BaseModel):
    """Status of a fsb template build."""

    phase: str = Field(description="Build lifecycle phase (see TemplatePhase).")
    manifest_ref: str | None = Field(
        default=None,
        alias="manifestRef",
        description=(
            "S3 manifest reference of the published artifacts; "
            "present when the phase is Succeeded."
        ),
    )
    message: str | None = Field(
        default=None,
        description="Failure reason when the phase is Failed.",
    )

    model_config = ConfigDict(populate_by_name=True)


class CreateTemplateRequest(BaseModel):
    """
    Request to create a fsb (fast-sandbox) template: a golden-image build.

    The build runs asynchronously: the response starts at ``phase: Pending``;
    poll ``get_template`` until the phase is ``Succeeded`` (or ``Failed``).
    Kernel, execd and guest init are server-side build inputs, not client
    fields.
    """

    image: str = Field(
        description="Source OCI image reference the golden image is built from."
    )
    publish: str = Field(
        description=(
            "S3-compatible publish target for the built artifacts, "
            "e.g. 's3://bucket/publish'."
        )
    )
    resource_limits: dict[str, str] | None = Field(
        default=None,
        alias="resourceLimits",
        description=(
            "Guest machine sizing, e.g. {'cpu': '1', 'memory': '512Mi', "
            "'disk': '2Gi'}. Defaults when omitted: cpu '1', memory '512Mi', "
            "disk '2Gi'."
        ),
    )
    entrypoint: list[str] | None = Field(
        default=None,
        description=(
            "Guest business command (argv); empty defaults to "
            "['tail', '-f', '/dev/null']."
        ),
    )
    metadata: dict[str, str] | None = Field(
        default=None,
        description="Custom key-value metadata for management, filtering, and tagging.",
    )
    readiness: TemplateReadiness | None = Field(
        default=None,
        description="Build-side readiness gate.",
    )
    format: Literal["native", "overlaybd"] | None = Field(
        default=None,
        description="Storage encoding of the produced snapshot set. Defaults to overlaybd.",
    )

    model_config = ConfigDict(populate_by_name=True)

    @field_validator("image", "publish")
    @classmethod
    def must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Template image and publish target cannot be blank")
        return v


class TemplateInfo(BaseModel):
    """
    A fsb template: a golden image whose build is declared and executed by
    fast-sandbox.
    """

    template_id: str = Field(
        description="Server-generated template ID ('tpl_<uuid>').",
        alias="templateId",
    )
    image: str = Field(description="Source OCI image reference.")
    publish: str = Field(description="S3-compatible publish target.")
    format: Literal["native", "overlaybd"] = Field(
        description="Snapshot storage encoding."
    )
    status: TemplateStatus = Field(description="Current build status.")
    created_at: datetime = Field(description="Creation timestamp (RFC 3339 UTC).")
    updated_at: datetime = Field(description="Last update timestamp (RFC 3339 UTC).")
    resource_limits: dict[str, str] | None = Field(
        default=None,
        alias="resourceLimits",
        description="Guest machine sizing (cpu/memory/disk).",
    )
    entrypoint: list[str] | None = Field(
        default=None,
        description="Guest business command (argv).",
    )
    metadata: dict[str, str] | None = Field(
        default=None,
        description="Custom metadata from the creation request.",
    )
    readiness: TemplateReadiness | None = Field(
        default=None,
        description="Build-side readiness gate.",
    )

    model_config = ConfigDict(populate_by_name=True)


class TemplateFilter(BaseModel):
    """
    Filter criteria for listing templates.
    """

    metadata: dict[str, str] | None = Field(
        default=None,
        description="Filter by metadata key-value pairs (AND logic).",
    )
    page_size: int | None = Field(
        default=None,
        description="Number of items per page (1-200).",
        alias="pageSize",
    )
    page: int | None = Field(
        default=None,
        description="Page number (1-indexed).",
    )

    model_config = ConfigDict(populate_by_name=True)

    @field_validator("page_size")
    @classmethod
    def template_page_size_must_be_positive(cls, v: int | None) -> int | None:
        if v is not None and v <= 0:
            raise ValueError("Page size must be positive")
        return v

    @field_validator("page")
    @classmethod
    def template_page_must_be_positive(cls, v: int | None) -> int | None:
        if v is not None and v < 1:
            raise ValueError("Page must be at least 1 (1-indexed)")
        return v


class PagedTemplateInfos(BaseModel):
    """
    A paginated list of template information.
    """

    template_infos: list[TemplateInfo] = Field(
        description="List of template details for the current page.",
    )
    pagination: PaginationInfo = Field(description="Pagination metadata.")

    model_config = ConfigDict(populate_by_name=True)
