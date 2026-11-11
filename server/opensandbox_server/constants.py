# Copyright 2026 The OpenSandbox Authors
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

"""Dependency-free constants shared across server layers."""

# Server-injected transport for execd lifecycle hooks; not user-settable.
OPENSANDBOX_LIFECYCLE = "OPENSANDBOX_LIFECYCLE"

# OSEP-0024 idle-policy bounds, shared by the API schema (pydantic Field
# bounds) and the idle-pause integration (validation + annotation codec).
# Defined on this leaf module because api/schema and the idle_pause package
# import each other transitively.
IDLE_TIMEOUT_MIN_SECONDS = 30
IDLE_TIMEOUT_MAX_SECONDS = 86400
PROBE_TIMEOUT_MIN_SECONDS = 1
PROBE_TIMEOUT_MAX_SECONDS = 10
DEFAULT_PROBE_TIMEOUT_SECONDS = 2

# OSEP-0009 renew-on-access bounds, shared by the API schema and the
# extensions validation (re-exported as ACCESS_RENEW_EXTEND_SECONDS_MIN/MAX).
RENEW_EXTEND_MIN_SECONDS = 300
RENEW_EXTEND_MAX_SECONDS = 86400
