#
# Copyright 2025 The OpenSandbox Authors
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
Converter for code execution models between domain and API layers.

Handles the transformation of code execution requests and contexts
between the domain model and auto-generated API client models.
"""

from typing import Any

from opensandbox.api.execd.models import CodeContext as ApiCodeContext

from code_interpreter.models.code import CodeContext


class CodeExecutionConverter:
    """
    Converts code execution models between domain and API representations.
    """

    @staticmethod
    def to_api_run_code_request(code: str, context: CodeContext | None) -> dict[str, Any]:
        """
        Converts domain code + context to API request dictionary.

        Args:
            code: Source code to execute
            context: Optional execution context (language + optional id)

        Returns:
            Dictionary representation for API call
        """
        result: dict[str, Any] = {"code": code}

        if context is not None:
            result["context"] = CodeExecutionConverter.to_api_code_context(context)

        return result

    @staticmethod
    def to_api_code_context(context: CodeContext) -> dict[str, Any]:
        """
        Converts domain CodeContext to API context dictionary.

        Args:
            context: Domain model code context

        Returns:
            Dictionary representation for API call
        """
        result: dict[str, Any] = {
            "language": context.language,
        }

        if context.id:
            result["id"] = context.id

        return result

    @staticmethod
    def from_api_code_context(api_context: ApiCodeContext) -> CodeContext:
        """
        Converts API CodeContextResponse to domain CodeContext.

        Args:
            api_context: API response from create_code_context

        Returns:
            Domain model code context
        """
        from opensandbox.api.execd.types import Unset

        context_id = None if isinstance(api_context.id, Unset) else api_context.id

        return CodeContext(
            id=context_id,
            language=api_context.language
        )

    @staticmethod
    def from_api_code_context_dict(api_context: dict[str, Any]) -> CodeContext:
        """
        Converts API code context dictionary to domain CodeContext.

        Args:
            api_context: API response dictionary containing context data

        Returns:
            Domain model code context
        """
        return CodeContext(
            id=api_context.get("id"),
            language=api_context.get("language", "python")
        )
