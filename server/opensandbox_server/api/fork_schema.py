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

"""Public models for a single, rootfs-only sandbox fork."""

from datetime import datetime
from typing import Literal, Optional

from pydantic import BaseModel, ConfigDict, Field, model_validator

from opensandbox_server.api.schema import NetworkPolicy, ResourceLimits, SandboxStatus


class ForkOverrides(BaseModel):
    model_config = ConfigDict(populate_by_name=True, extra="forbid")

    env: Optional[dict[str, Optional[str]]] = None
    resource_limits: Optional[ResourceLimits] = Field(None, alias="resourceLimits")
    resource_requests: Optional[ResourceLimits] = Field(None, alias="resourceRequests")
    network_policy: Optional[NetworkPolicy] = Field(None, alias="networkPolicy")
    metadata: Optional[dict[str, str]] = None
    entrypoint: Optional[list[str]] = Field(None, min_length=1)

    @model_validator(mode="after")
    def reject_null_overrides(self) -> "ForkOverrides":
        if any(getattr(self, name) is None for name in self.model_fields_set):
            raise ValueError("Fork overrides must not be null; omit a field to inherit it.")
        return self


class ForkSandboxRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    timeout: int = Field(..., ge=60, description="Target sandbox lifetime in seconds.")
    overrides: ForkOverrides = Field(default_factory=lambda: ForkOverrides.model_validate({}))


class ForkOperation(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    id: str
    source_sandbox_id: str = Field(..., alias="sourceSandboxId")
    status: SandboxStatus
    snapshot_id: Optional[str] = Field(None, alias="snapshotId")
    sandbox_id: Optional[str] = Field(None, alias="sandboxId")
    created_at: datetime = Field(..., alias="createdAt")
    updated_at: datetime = Field(..., alias="updatedAt")
    cleanup_pending: bool = Field(False, alias="cleanupPending")


ForkState = Literal["Pending", "Snapshotting", "Provisioning", "Succeeded", "Failed"]
