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
"""Diagnostic payload models."""

from datetime import datetime
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field


class DiagnosticContent(BaseModel):
    """Descriptor for best-effort plain-text diagnostic content."""

    sandbox_id: str = Field(alias="sandboxId")
    kind: Literal["logs", "events"]
    scope: str
    delivery: Literal["inline", "url"]
    content_type: str = Field(alias="contentType")
    truncated: bool
    content: str | None = None
    content_url: str | None = Field(default=None, alias="contentUrl")
    content_length: int | None = Field(default=None, alias="contentLength")
    expires_at: datetime | None = Field(default=None, alias="expiresAt")
    warnings: list[str] | None = None

    model_config = ConfigDict(populate_by_name=True)
