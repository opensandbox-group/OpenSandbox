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
Synchronous service interfaces (Protocols) for the Code Interpreter sync SDK.

These interfaces mirror the async interfaces under :mod:`code_interpreter.services`,
but are **blocking** and intended for use with :class:`code_interpreter.sync.code_interpreter.CodeInterpreterSync`.
"""

from code_interpreter.sync.services.code import CodesSync

__all__ = [
    "CodesSync",
]
