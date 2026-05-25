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
EventNode model for parsing Server-Sent Events from execd.
"""

from typing import Any

from pydantic import BaseModel, ConfigDict, Field, field_validator


class EventNodeError(BaseModel):
    """Error details in an event node."""

    name: str | None = Field(default=None, alias="ename")
    value: str | None = Field(default=None, alias="evalue")
    traceback: list[str] = Field(default_factory=list)

    @field_validator("traceback", mode="before")
    @classmethod
    def normalize_traceback(cls, value: Any) -> list[str]:
        """Normalize older or malformed SSE payloads into a stable list form."""
        if value is None:
            return []
        if isinstance(value, list):
            return [str(item) for item in value]
        if isinstance(value, tuple):
            return [str(item) for item in value]
        return [str(value)]


class EventNodeResults(BaseModel):
    """Results container in an event node."""

    text: str | None = Field(default=None, alias="text")

    def get_text(self) -> str:
        """Get the text representation of the result."""
        return self.text or ""

    model_config = ConfigDict(extra="allow")  # Allow other mime types


class EventNode(BaseModel):
    """
    Represents a single event from the server stream.
    Corresponds to ServerStreamEvent in OpenAPI spec.
    """

    type: str
    text: str | None = None
    execution_count: int | None = Field(default=None, alias="execution_count")
    execution_time_in_millis: int | None = Field(default=None, alias="execution_time")
    timestamp: int
    results: EventNodeResults | None = None
    error: EventNodeError | None = None
