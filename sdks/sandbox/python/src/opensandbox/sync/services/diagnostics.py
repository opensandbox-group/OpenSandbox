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
"""Synchronous diagnostics service interface."""

from typing import Protocol

from opensandbox.models.diagnostics import DiagnosticContent


class DiagnosticsSync(Protocol):
    """Synchronous sandbox diagnostics service."""

    def get_logs(
        self,
        sandbox_id: str,
        scope: str,
    ) -> DiagnosticContent:
        """Get diagnostic log content descriptor."""
        ...

    def get_events(
        self,
        sandbox_id: str,
        scope: str,
    ) -> DiagnosticContent:
        """Get diagnostic event content descriptor."""
        ...
