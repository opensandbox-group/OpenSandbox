#
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
#
"""Shared header helpers for lifecycle control-plane requests."""

from collections.abc import Mapping

LIFECYCLE_API_KEY_HEADER = "OPEN-SANDBOX-API-KEY"


def has_header(headers: Mapping[str, str], name: str) -> bool:
    """Return True if ``headers`` contains ``name`` (case-insensitive)."""
    target = name.lower()
    return any(existing.lower() == target for existing in headers)


def apply_default_lifecycle_api_key(
    headers: Mapping[str, str],
    api_key: str | None,
) -> dict[str, str]:
    """Return a copy of ``headers`` with the default API key unless explicitly set."""
    resolved = dict(headers)
    if api_key and not has_header(resolved, LIFECYCLE_API_KEY_HEADER):
        resolved[LIFECYCLE_API_KEY_HEADER] = api_key
    return resolved
