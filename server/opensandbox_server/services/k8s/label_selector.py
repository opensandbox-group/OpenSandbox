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

"""Minimal Kubernetes label selector parser for in-memory matching.

Supports only the subset that callers in this codebase actually emit:

- empty string ............ matches every object
- ``key`` ................. key existence
- ``key=value`` ........... equality (``==`` accepted as alias)
- ``a=1,b=2`` ............. comma-joined AND of the above

When the selector contains anything outside this grammar (set-based ops
like ``in``, ``notin``, ``!key``), :func:`parse_selector` returns ``None``
so the caller falls back to issuing a real Kubernetes API list request.
"""

from __future__ import annotations

from typing import List, Literal, Mapping, Optional, Tuple

Op = Literal["exists", "eq"]
Term = Tuple[str, Op, Optional[str]]


_LABEL_KEY_CHARS = set(
    "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./"
)


def _is_valid_key(key: str) -> bool:
    if not key:
        return False
    return all(c in _LABEL_KEY_CHARS for c in key)


def parse_selector(selector: str) -> Optional[List[Term]]:
    """Parse a label selector into a list of AND terms.

    Returns ``None`` when the selector uses syntax beyond what this minimal
    parser supports. The empty selector parses to ``[]`` (match-all).
    """
    selector = (selector or "").strip()
    if not selector:
        return []

    terms: List[Term] = []
    for raw in selector.split(","):
        clause = raw.strip()
        if not clause:
            return None

        if "==" in clause:
            key, _, value = clause.partition("==")
        elif "=" in clause:
            key, _, value = clause.partition("=")
        else:
            key, value = clause, None

        key = key.strip()
        if not _is_valid_key(key):
            return None
        if value is None:
            terms.append((key, "exists", None))
        else:
            terms.append((key, "eq", value.strip()))

    return terms


def matches(labels: Mapping[str, str], terms: List[Term]) -> bool:
    """Return True if ``labels`` satisfy every AND term."""
    for key, op, expected in terms:
        if op == "exists":
            if key not in labels:
                return False
        elif op == "eq":
            if labels.get(key) != expected:
                return False
        else:  # pragma: no cover - exhaustive on Op
            return False
    return True
