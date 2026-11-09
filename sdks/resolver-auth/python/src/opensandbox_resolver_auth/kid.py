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

"""Platform identifier grammar for JWT ``kid`` values (OSEP-0025).

The grammar is enforced locally, before any JWKS fetch: an off-grammar
kid is rejected with zero network activity, so random-kid spraying cannot
amplify into lifecycle-server lookups (OSEP line 450). Both profiles are
covered — sidecar sandbox ids (optionally carrying an incarnation suffix)
and fast-sandbox ``<pool>:<version>`` keys. Every allowed character is
path-safe, so a valid kid substitutes into the ``/jwks/{kid}`` lookup
path without escaping ambiguity.
"""

from __future__ import annotations

import re

__all__ = ["is_valid_kid", "KID_MAX_LENGTH", "KID_PATTERN"]

# One identifier segment: 1–63 characters, starting alphanumeric, then
# alphanumerics plus ``_``, ``.``, ``-``. A fast-sandbox kid joins two
# segments with exactly one ``:``.
KID_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,62}(?::[A-Za-z0-9][A-Za-z0-9_.-]{0,62})?")
KID_MAX_LENGTH = 127


def is_valid_kid(kid: object) -> bool:
    """Return whether ``kid`` matches the platform identifier grammar."""
    if not isinstance(kid, str) or not kid or len(kid) > KID_MAX_LENGTH:
        return False
    return KID_PATTERN.fullmatch(kid) is not None
