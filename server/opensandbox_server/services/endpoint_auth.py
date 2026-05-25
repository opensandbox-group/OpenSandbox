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

"""Helpers for sandbox endpoint authentication."""

from __future__ import annotations

import secrets

from opensandbox_server.services.constants import OPEN_SANDBOX_EGRESS_AUTH_HEADER, OPEN_SANDBOX_SECURE_ACCESS_HEADER

EGRESS_AUTH_TOKEN_BYTES = 24
SECURE_ACCESS_TOKEN_BYTES = 24


def generate_egress_token() -> str:
    """Return a random URL-safe token for egress endpoint auth."""
    return secrets.token_urlsafe(EGRESS_AUTH_TOKEN_BYTES)


def generate_secure_access_token() -> str:
    """Return a random URL-safe token for sandbox secure access."""
    return secrets.token_urlsafe(SECURE_ACCESS_TOKEN_BYTES)


def build_egress_auth_headers(token: str) -> dict[str, str]:
    """Build endpoint headers for egress auth."""
    return {OPEN_SANDBOX_EGRESS_AUTH_HEADER: token}


def build_secure_access_headers(token: str) -> dict[str, str]:
    """Build endpoint headers for sandbox secure access."""
    return {OPEN_SANDBOX_SECURE_ACCESS_HEADER: token}


def merge_endpoint_headers(
    existing: dict[str, str] | None,
    extra: dict[str, str],
) -> dict[str, str]:
    """Merge auth headers into existing endpoint headers without mutating input."""
    merged: dict[str, str] = dict(existing or {})
    merged.update(extra)
    return merged


__all__ = [
    "EGRESS_AUTH_TOKEN_BYTES",
    "SECURE_ACCESS_TOKEN_BYTES",
    "build_egress_auth_headers",
    "build_secure_access_headers",
    "generate_egress_token",
    "generate_secure_access_token",
    "merge_endpoint_headers",
]
