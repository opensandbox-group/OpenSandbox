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
Model converter utilities for API/domain model mapping.

This package provides:
- ExceptionConverter: Convert various exceptions to SandboxException
- ResponseHandler: Unified API response handling
- SandboxModelConverter: Convert between API and domain models
- FilesystemModelConverter: Convert filesystem-related models
- MetricsModelConverter: Convert metrics-related models
- ExecutionConverter: Convert execution-related models
"""

from opensandbox.adapters.converter.command_model_converter import (
    to_command_status,
)
from opensandbox.adapters.converter.exception_converter import (
    ExceptionConverter,
    parse_sandbox_error,
)
from opensandbox.adapters.converter.filesystem_model_converter import (
    FilesystemModelConverter,
)
from opensandbox.adapters.converter.metrics_model_converter import (
    MetricsModelConverter,
)
from opensandbox.adapters.converter.response_handler import (
    handle_api_error,
)
from opensandbox.adapters.converter.sandbox_model_converter import (
    SandboxModelConverter,
)

__all__ = [
    "ExceptionConverter",
    "parse_sandbox_error",
    "FilesystemModelConverter",
    "MetricsModelConverter",
    "to_command_status",
    "SandboxModelConverter",
    "handle_api_error",
]
