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
"""Converters for diagnostics API models."""

from opensandbox.api.diagnostic.models import (
    DiagnosticContentResponse as ApiDiagnosticContentResponse,
)
from opensandbox.models.diagnostics import DiagnosticContent


class DiagnosticModelConverter:
    """Convert generated diagnostics API models to public SDK models."""

    @staticmethod
    def to_diagnostic_content(
        response: ApiDiagnosticContentResponse,
    ) -> DiagnosticContent:
        return DiagnosticContent.model_validate(response.to_dict())
