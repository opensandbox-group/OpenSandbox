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
Code execution models.

Models for code contexts, execution requests, and language support.
"""


from pydantic import BaseModel, ConfigDict, Field, field_validator


class SupportedLanguage:
    """
    Supported programming languages for code execution.

    This class defines the languages that are officially supported by the code interpreter.
    When adding new languages, ensure corresponding execution environments are available.
    """
    PYTHON = "python"
    JAVA = "java"
    GO = "go"
    TYPESCRIPT = "typescript"
    BASH = "bash"
    JAVASCRIPT = "javascript"


class CodeContext(BaseModel):
    """
    Represents an execution context for code interpretation.

    A CodeContext maintains the execution environment for a specific programming
    language, including the working directory, language configuration, and
    persistent state across multiple code executions.

    Context Lifecycle:

    1. Creation: Context is created with language and working directory
    2. Execution: Code runs within this context, building up state
    3. Persistence: Variables, imports, and functions persist between executions
    4. Cleanup: Context is destroyed explicitly by the caller when no longer needed
    """

    id: str | None = Field(default=None, description="Unique identifier for this execution context")
    language: str = Field(description="Programming language for this context (e.g., 'python', 'javascript')")

    @field_validator('language')
    @classmethod
    def language_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Language cannot be blank")
        return v


    model_config = ConfigDict(arbitrary_types_allowed=True)
