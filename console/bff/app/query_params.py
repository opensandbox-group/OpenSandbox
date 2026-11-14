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

from __future__ import annotations

from fastapi import HTTPException, status


def parse_query_int(
    raw: str | None,
    *,
    field: str,
    default: int,
    minimum: int = 1,
    maximum: int,
) -> int:
    if raw is None or str(raw).strip() == "":
        value = default
    else:
        try:
            value = int(str(raw).strip())
        except ValueError as exc:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": "INVALID_QUERY",
                    "message": f"{field} must be an integer",
                },
            ) from exc
    if value < minimum:
        value = minimum
    if value > maximum:
        value = maximum
    return value


def parse_archive_max_bytes(raw: str | None, *, cap: int) -> int:
    """Parse maxBytes query; always clamp to operator cap (1..cap)."""
    if cap < 1:
        cap = 1
    if raw is None or str(raw).strip() == "":
        return cap
    try:
        value = int(str(raw).strip())
    except ValueError as exc:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": "INVALID_QUERY",
                "message": "maxBytes must be an integer",
            },
        ) from exc
    if value < 1:
        value = 1
    return min(value, cap)
