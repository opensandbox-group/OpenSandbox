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

"""Request envelopes for native Kubernetes batch resources."""

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

from opensandbox_server.services.constants import SANDBOX_TENANT_LABEL

BATCH_MANAGED_LABEL = "opensandbox.io/batch-managed-by"
RESERVED_LABELS = {BATCH_MANAGED_LABEL, SANDBOX_TENANT_LABEL}
RESOURCE_NAME_PATTERN = r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"


class BatchMetadata(BaseModel):
    model_config = ConfigDict(extra="forbid")

    name: str = Field(min_length=1, max_length=63, pattern=RESOURCE_NAME_PATTERN)
    namespace: str | None = None
    labels: dict[str, str] = Field(default_factory=dict)
    annotations: dict[str, str] = Field(default_factory=dict)

    @model_validator(mode="after")
    def validate_labels(self) -> "BatchMetadata":
        if RESERVED_LABELS.intersection(self.labels):
            raise ValueError("Batch ownership labels are reserved for the server.")
        return self


class JobManifest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    api_version: Literal["batch/v1"] = Field(alias="apiVersion")
    kind: Literal["Job"]
    metadata: BatchMetadata
    spec: dict[str, Any]


class CronJobManifest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    api_version: Literal["batch/v1"] = Field(alias="apiVersion")
    kind: Literal["CronJob"]
    metadata: BatchMetadata
    spec: dict[str, Any]

    @model_validator(mode="after")
    def validate_job_template(self) -> "CronJobManifest":
        if len(self.metadata.name) > 52:
            raise ValueError("CronJob names must be at most 52 characters.")
        template = self.spec.get("jobTemplate")
        if not isinstance(template, dict) or not isinstance(template.get("spec"), dict):
            raise ValueError("spec.jobTemplate.spec must be an object.")
        metadata = template.get("metadata", {})
        if not isinstance(metadata, dict):
            raise ValueError("spec.jobTemplate.metadata must be an object.")
        # Only user metadata is accepted on the generated Job template as well.
        if set(metadata) - {"labels", "annotations"}:
            raise ValueError("Job template metadata accepts only labels and annotations.")
        BatchMetadata(name=self.metadata.name, **metadata)
        return self
