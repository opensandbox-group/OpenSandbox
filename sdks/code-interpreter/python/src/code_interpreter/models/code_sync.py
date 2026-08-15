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
Synchronous code execution models for Code Interpreter SDK.
"""

from pydantic import BaseModel, Field, field_validator


class SupportedLanguageSync:
    # Mirror of SupportedLanguage; note that JAVASCRIPT is intentionally
    # absent because the sync flow only wires the languages listed below.
    PYTHON = "python"
    JAVA = "java"
    GO = "go"
    TYPESCRIPT = "typescript"
    BASH = "bash"


class CodeContextSync(BaseModel):
    id: str | None = Field(default=None)
    language: str = Field(description="Programming language for this context")

    @field_validator("language")
    @classmethod
    def language_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Language cannot be blank")
        return v
