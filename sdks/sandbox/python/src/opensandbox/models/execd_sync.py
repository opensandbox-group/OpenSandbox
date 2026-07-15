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
Synchronous execution-related models.

This mirrors `opensandbox.models.execd` but uses synchronous handlers.
Core data models (Execution, OutputMessage, etc.) are reused from the async module.
"""

from collections.abc import Callable
from typing import Any

from pydantic import BaseModel, ConfigDict, Field

SyncOutputHandler = Callable[[Any], None]


class ExecutionHandlersSync(BaseModel):
    """
    Synchronous handlers for streaming execution output.
    """

    on_stdout: SyncOutputHandler | None = Field(default=None)
    on_stderr: SyncOutputHandler | None = Field(default=None)
    on_result: SyncOutputHandler | None = Field(default=None)
    on_execution_complete: SyncOutputHandler | None = Field(default=None, alias="on_execution_complete")
    on_error: SyncOutputHandler | None = Field(default=None)
    on_init: SyncOutputHandler | None = Field(default=None)
    skip_accumulation: bool = Field(
        default=False,
        description="When True, stdout/stderr messages are only delivered to handlers "
        "without being accumulated in ExecutionLogs.",
    )

    model_config = ConfigDict(populate_by_name=True, arbitrary_types_allowed=True)
