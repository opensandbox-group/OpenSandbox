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

"""Domain models for the fsb template catalog.

A template row is the server-side source of truth for the public catalog;
the fast-sandbox ``SandboxTemplate`` CRD is the execution projection of the
stored build intent.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Optional


class FsbTemplatePhase(str, Enum):
    """Lifecycle of a template build, mirroring the CRD status phase."""

    PENDING = "Pending"
    BUILDING = "Building"
    SUCCEEDED = "Succeeded"
    FAILED = "Failed"


@dataclass
class FsbTemplateRecord:
    """One persisted template row."""

    template_id: str
    namespace: str
    crd_name: str
    spec: dict  # normalized build intent (image, entrypoint, resourceLimits, readiness, publish, format)
    metadata: dict[str, str] = field(default_factory=dict)
    phase: FsbTemplatePhase = FsbTemplatePhase.PENDING
    manifest_ref: Optional[str] = None
    message: Optional[str] = None
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None

    @property
    def source_image(self) -> str:
        return str(self.spec.get("image", ""))

    @property
    def publish(self) -> str:
        return str(self.spec.get("publish", ""))


@dataclass
class FsbTemplateListQuery:
    """Tenant-scoped list query evaluated against the store."""

    namespace: str
    metadata: Optional[dict[str, str]] = None
    page: int = 1
    page_size: int = 20


@dataclass
class FsbTemplateListResult:
    items: list[FsbTemplateRecord]
    total_items: int


__all__ = [
    "FsbTemplateListQuery",
    "FsbTemplateListResult",
    "FsbTemplatePhase",
    "FsbTemplateRecord",
]
