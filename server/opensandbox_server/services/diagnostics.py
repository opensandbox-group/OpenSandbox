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

"""Runtime-neutral results and helpers for stable sandbox diagnostics."""

from dataclasses import dataclass
from typing import Literal

from fastapi import HTTPException, status

DiagnosticKind = Literal["logs", "events"]


@dataclass(frozen=True, slots=True)
class DiagnosticResult:
    """Stable diagnostic content collected by a runtime service."""

    sandbox_id: str
    kind: DiagnosticKind
    scope: str
    content: str
    truncated: bool = False
    warnings: tuple[str, ...] = ()


def limit_diagnostic_lines(
    content: str,
    limit: int,
    *,
    keep_tail: bool,
) -> tuple[str, bool]:
    """Apply a line limit after collecting one extra line.

    Args:
        content: Diagnostic text to bound.
        limit: Maximum number of lines to return.
        keep_tail: Keep the newest trailing lines when true; otherwise keep the
            leading lines.

    Returns:
        The bounded content and whether truncation occurred.
    """
    lines = content.splitlines(keepends=True)
    if len(lines) <= limit:
        return content, False
    bounded_lines = lines[-limit:] if keep_tail else lines[:limit]
    return "".join(bounded_lines), True


def unsupported_scope_error(
    kind: DiagnosticKind,
    scope: str,
    supported: tuple[str, ...],
) -> HTTPException:
    """Build the stable error for a scope unsupported by a runtime.

    Args:
        kind: Diagnostic payload kind.
        scope: Scope requested by the caller.
        supported: Scopes implemented by the runtime.

    Returns:
        An HTTP 400 exception matching the diagnostics error contract.
    """
    return HTTPException(
        status_code=status.HTTP_400_BAD_REQUEST,
        detail={
            "code": "DIAGNOSTICS_SCOPE_UNSUPPORTED",
            "message": (
                f"Unsupported {kind} diagnostics scope {scope!r}. "
                f"Supported scopes: {', '.join(supported)}."
            ),
        },
    )
