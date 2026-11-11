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
OpenSandbox Code Interpreter SDK.

This package provides secure, isolated code execution capabilities built on top
of the OpenSandbox infrastructure. It supports multiple programming languages,
session management, and variable persistence across executions.
"""

from importlib.metadata import PackageNotFoundError
from importlib.metadata import version as _pkg_version

from code_interpreter.code_interpreter import CodeInterpreter
from code_interpreter.models.code import (
    CodeContext,
    SupportedLanguage,
)
from code_interpreter.sync.code_interpreter import CodeInterpreterSync

__all__ = [
    "CodeInterpreter",
    "CodeInterpreterSync",
    "CodeContext",
    "SupportedLanguage",
]

try:
    __version__ = _pkg_version("opensandbox-code-interpreter")
except PackageNotFoundError:  # pragma: no cover
    # Fallback for editable/uninstalled source checkouts.
    __version__ = "0.0.0"
