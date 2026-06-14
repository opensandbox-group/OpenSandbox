#
# Copyright 2025 Alibaba Group Holding Ltd.
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
Pool-related domain models.

Models for pool creation, configuration, and status management.
Pools represent pre-warmed sets of sandbox pods that reduce cold-start latency.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

from pydantic import BaseModel, ConfigDict, Field


class PoolCapacitySpec(BaseModel):
    """
    Capacity configuration for a pre-warmed pool.

    Controls how many pods are kept warm and the overall pool size limits.
    """

    model_config = ConfigDict(populate_by_name=True)

    buffer_max: int = Field(
        ...,
        alias="bufferMax",
        ge=0,
        description="Maximum number of pods kept in the warm buffer.",
    )
    buffer_min: int = Field(
        ...,
        alias="bufferMin",
        ge=0,
        description="Minimum number of pods that must remain in the buffer.",
    )
    pool_max: int = Field(
        ...,
        alias="poolMax",
        ge=0,
        description="Maximum total number of pods allowed in the pool.",
    )
    pool_min: int = Field(
        ...,
        alias="poolMin",
        ge=0,
        description="Minimum total size of the pool.",
    )


class PoolStatus(BaseModel):
    """Observed runtime state of a pool reported by the controller."""

    total: int = Field(..., description="Total number of pods in the pool.")
    allocated: int = Field(..., description="Number of pods currently allocated to sandboxes.")
    available: int = Field(..., description="Number of pods currently available in the warm buffer.")
    revision: str = Field(..., description="Latest revision identifier of the pool spec.")


class PoolInfo(BaseModel):
    """
    Complete representation of a Pool resource.

    Returned by create/get/update operations and as items in list responses.
    """

    model_config = ConfigDict(populate_by_name=True)

    name: str = Field(..., description="Unique pool name (Kubernetes resource name).")
    capacity_spec: PoolCapacitySpec = Field(
        ...,
        alias="capacitySpec",
        description="Capacity configuration of the pool.",
    )
    status: PoolStatus | None = Field(
        None,
        description="Observed runtime state. May be None if not yet reconciled by the controller.",
    )
    created_at: datetime | None = Field(
        None,
        alias="createdAt",
        description="Pool creation timestamp.",
    )


class PoolListResponse(BaseModel):
    """Response from listing pools."""

    items: list[PoolInfo] = Field(..., description="List of pools.")


class CreatePoolParams(BaseModel):
    """
    Parameters for creating a new Pool.

    Usage::

        params = CreatePoolParams(
            name="my-pool",
            template={"spec": {"containers": [{"name": "sandbox", "image": "python:3.11"}]}},
            capacity_spec=PoolCapacitySpec(bufferMax=3, bufferMin=1, poolMax=10, poolMin=0),
        )
        pool = await manager.create_pool(params)
    """

    model_config = ConfigDict(populate_by_name=True)

    name: str = Field(
        ...,
        description="Unique name for the pool (must be a valid Kubernetes resource name).",
        pattern=r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$",
        max_length=253,
    )
    template: dict[str, Any] = Field(
        ...,
        description=(
            "Kubernetes PodTemplateSpec defining the pod configuration for pre-warmed pods. "
            "Follows the same schema as spec.template in a Kubernetes Deployment."
        ),
    )
    capacity_spec: PoolCapacitySpec = Field(
        ...,
        alias="capacitySpec",
        description="Initial capacity configuration controlling pool size and buffer behavior.",
    )


class UpdatePoolParams(BaseModel):
    """
    Parameters for updating an existing Pool's capacity.

    Only ``capacity_spec`` can be updated after pool creation.
    To change the pod template, delete and recreate the pool.

    Usage::

        params = UpdatePoolParams(
            capacity_spec=PoolCapacitySpec(bufferMax=5, bufferMin=2, poolMax=20, poolMin=0)
        )
        updated = await manager.update_pool("my-pool", params)
    """

    model_config = ConfigDict(populate_by_name=True)

    capacity_spec: PoolCapacitySpec = Field(
        ...,
        alias="capacitySpec",
        description="New capacity configuration for the pool.",
    )
