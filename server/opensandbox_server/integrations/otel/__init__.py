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

"""Optional OpenTelemetry integration for SDK metrics ingestion."""

from opensandbox_server.integrations.otel.metrics import (
    record_sandbox_create_duration,
    setup_otel_metrics,
    shutdown_otel_metrics,
)

__all__ = [
    "record_sandbox_create_duration",
    "setup_otel_metrics",
    "shutdown_otel_metrics",
]
